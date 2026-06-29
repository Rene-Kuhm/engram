package store

import (
	"context"
	"testing"
)

// TestRecoverTruncated_ReturnsSoftDeletedTurns covers the PR4 / REQ-007
// recovery story: after truncate, descendants MUST be re-discoverable
// so the caller can re-fork them via ForkSession. The helper MUST
// return exactly the descendant rows, ordered deterministically by
// turn_seq ASC, with their full metadata (the markers that the truncate
// stamped plus the user-provided fields) preserved.
func TestRecoverTruncated_ReturnsSoftDeletedTurns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const sid = "recover-trunc-data"
	const proj = "proj-X"
	ids := seedLinearSessionForTruncate(t, s, sid, proj)
	// Truncate at turn 3 (ids[2]). Descendants: ids[3], ids[4].
	target := ids[2]

	if _, err := s.RewindSession(ctx, RewindSessionParams{
		SessionID:       sid,
		AtTurnID:        target,
		Mode:            RewindModeTruncate,
		ConfirmTruncate: true,
		FromProject:     proj,
	}); err != nil {
		t.Fatalf("RewindSession truncate: %v", err)
	}

	recovered, err := s.RecoverTruncated(ctx, sid, proj)
	if err != nil {
		t.Fatalf("RecoverTruncated: %v", err)
	}
	if len(recovered) != 2 {
		t.Fatalf("recovered %d turns, want 2 (ids[3], ids[4])", len(recovered))
	}
	if recovered[0].ID != ids[3] || recovered[1].ID != ids[4] {
		t.Errorf("recovered order: got [%s, %s], want [%s, %s]",
			recovered[0].ID, recovered[1].ID, ids[3], ids[4])
	}
	for _, r := range recovered {
		if r.Metadata == nil {
			t.Errorf("recovered turn %s has nil metadata; want the truncated markers intact",
				r.ID)
			continue
		}
		if got, _ := r.Metadata["truncated_at_turn_id"].(string); got != target {
			t.Errorf("recovered %s truncated_at_turn_id = %q, want %q",
				r.ID, got, target)
		}
		if got, _ := r.Metadata["truncated_from_session_id"].(string); got != sid {
			t.Errorf("recovered %s truncated_from_session_id = %q, want %q",
				r.ID, got, sid)
		}
		if _, ok := r.Metadata["truncated_at"]; !ok {
			t.Errorf("recovered %s missing truncated_at unix_ms", r.ID)
		}
	}

	// Crucial invariant: RecoverTruncated MUST return ONLY truncated
	// descendants — never the kept-prefix rows. Pass the same session
	// and confirm the kept-prefix turns (ids[0..2]) are NOT present.
	for _, keptID := range []string{ids[0], ids[1], ids[2]} {
		for _, r := range recovered {
			if r.ID == keptID {
				t.Errorf("RecoverTruncated leaked kept-prefix turn %s", keptID)
			}
		}
	}
}

// TestRecoverTruncated_EmptyWhenNoneTruncated covers the no-op path:
// when a session has no truncated descendants, RecoverTruncated MUST
// return an empty slice with a nil error. This is the contract that
// makes CLI `engram session recover <sid>` print "(no truncated
// turns)" instead of erroring out.
func TestRecoverTruncated_EmptyWhenNoneTruncated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const sid = "recover-empty"
	const proj = "proj-Y"
	if _, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID: sid, Project: proj, Role: "user",
		ContentJSON: []byte(`[{"type":"text","text":"alive"}]`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	recovered, err := s.RecoverTruncated(ctx, sid, proj)
	if err != nil {
		t.Fatalf("RecoverTruncated (no truncate): %v", err)
	}
	if len(recovered) != 0 {
		t.Errorf("recovered %d turns, want 0 (no truncate occurred)", len(recovered))
	}
}

// TestRecoverTruncated_ReForkableViaForkSession is the end-to-end
// recovery story: a truncated descendant MUST be re-forkable into a
// fresh session via ForkSession, with the truncated_from_session_id
// marker intact on the original row so the audit trail is preserved.
// This is the contract that makes truncate "destructive but
// recoverable" rather than destructive-and-doomed.
func TestRecoverTruncated_ReForkableViaForkSession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const sid = "recover-refork"
	const proj = "proj-X"
	ids := seedLinearSessionForTruncate(t, s, sid, proj)
	target := ids[2] // truncate at turn 3 → descendants: ids[3], ids[4]

	if _, err := s.RewindSession(ctx, RewindSessionParams{
		SessionID:       sid,
		AtTurnID:        target,
		Mode:            RewindModeTruncate,
		ConfirmTruncate: true,
		FromProject:     proj,
	}); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Re-fork a truncated descendant onto a fresh session.
	newSID, _, err := s.ForkSession(ctx, ForkSessionParams{
		FromSessionID: sid,
		FromProject:   proj,
		AtTurnID:      ids[3],
	})
	if err != nil {
		t.Fatalf("ForkSession truncated descendant: %v", err)
	}
	if newSID == "" || newSID == sid {
		t.Errorf("ForkSession returned %q; want a fresh session id", newSID)
	}

	// The new session MUST have its own fresh id for ids[3], and the
	// kept-prefix clones must come along. Listing the new session
	// returns only the kept prefix (prefix turn 1..ids[3] inclusive).
	listed, err := s.ListTurns(ctx, newSID, ListTurnsOpts{})
	if err != nil {
		t.Fatalf("ListTurns on forked session: %v", err)
	}
	if len(listed) != 4 {
		t.Errorf("forked session has %d turns, want 4 (prefix 1..ids[3])", len(listed))
	}
	// ids[4] is the OTHER truncated descendant of ids[3]'s ancestor
	// (ids[2]); on a fresh forked session that chain is NOT inherited,
	// because ids[3] was the fork point. Verified by absence below.
	for _, r := range listed {
		if r.ID == ids[4] {
			t.Errorf("forked session leaked the orphaned descendant %s", ids[4])
		}
	}
}

// TestRecoverTruncated_OrderedByTurnSeq is a guard for ordering
// determinism: callers depend on turn_seq ASC to present rows in
// chronological order across recovery sessions, regardless of ULID
// entropy. The helper MUST sort by turn_seq ASC, not by id (ULID).
func TestRecoverTruncated_OrderedByTurnSeq(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const sid = "recover-order"
	const proj = "proj-X"
	ids := seedLinearSessionForTruncate(t, s, sid, proj)

	if _, err := s.RewindSession(ctx, RewindSessionParams{
		SessionID:       sid,
		AtTurnID:        ids[0], // truncate at turn 1 → descendants: ids[1..4]
		Mode:            RewindModeTruncate,
		ConfirmTruncate: true,
		FromProject:     proj,
	}); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	recovered, err := s.RecoverTruncated(ctx, sid, proj)
	if err != nil {
		t.Fatalf("RecoverTruncated: %v", err)
	}
	if len(recovered) != 4 {
		t.Fatalf("recovered %d turns, want 4", len(recovered))
	}
	for i := 0; i < len(recovered)-1; i++ {
		if recovered[i].TurnSeq >= recovered[i+1].TurnSeq {
			t.Errorf("RecoverTruncated returned out-of-order at [%d].seq=%d, [%d].seq=%d",
				i, recovered[i].TurnSeq, i+1, recovered[i+1].TurnSeq)
		}
	}
	// Defense in depth: ids in recovered output must match the seeded
	// descendants in the same position. Helper sorts by turn_seq ASC,
	// not by ULID, so the cross-check uses positional equality.
	for i, want := range []string{ids[1], ids[2], ids[3], ids[4]} {
		if recovered[i].ID != want {
			t.Errorf("recovered[%d].ID = %s, want %s (turn_seq ASC contract)",
				i, recovered[i].ID, want)
		}
	}
}

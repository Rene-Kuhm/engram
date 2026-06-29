package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestForkSession_FromMidpoint_PreservesContent covers BDD-S-006.a / §3.1:
// forking at a midpoint must:
//   - return a new session_id (different from the source)
//   - clone the prefix root..AtTurnID with byte-identical content_json
//   - reassign turn_seq to start at 1
//   - each clone gets a fresh ULID (turn ids in the new session are
//     independent of the source)
//   - the new root turn carries metadata.forked_from_session_id and
//     metadata.forked_from_turn_id
//   - the source session is unchanged (REQ-006: non-destructive)
func TestForkSession_FromMidpoint_PreservesContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const sourceSession = "src-s1"
	const sourceProject = "proj-X"

	// Build a 5-turn linear session with known content blocks. The fork
	// point is t3 — clones carry turns 1..3 byte-identical.
	var ids [5]string
	contents := [5][]byte{
		[]byte(`[{"type":"text","text":"t1 hello"}]`),
		[]byte(`[{"type":"text","text":"t2 world"}]`),
		[]byte(`[{"type":"tool-call","id":"call-1","name":"read","arguments":{"path":"a.go"}}]`),
		[]byte(`[{"type":"text","text":"t4"}]`),
		[]byte(`[{"type":"text","text":"t5"}]`),
	}
	for i := 0; i < 5; i++ {
		var parent *string
		if i > 0 {
			p := ids[i-1]
			parent = &p
		}
		saved, err := s.SaveTurn(ctx, SaveTurnParams{
			SessionID:    sourceSession,
			Project:      sourceProject,
			Role:         "user",
			ParentTurnID: parent,
			ContentJSON:  contents[i],
		})
		if err != nil {
			t.Fatalf("seed turn %d: %v", i+1, err)
		}
		ids[i] = saved.ID
	}
	targetID := ids[2]

	// Count source rows before fork for the no-mutation assertion.
	var preCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sourceSession,
	).Scan(&preCount); err != nil {
		t.Fatalf("pre-count source: %v", err)
	}

	// Fork at t3.
	newSID, newTurns, err := s.ForkSession(ctx, ForkSessionParams{
		FromSessionID: sourceSession,
		FromProject:   sourceProject,
		AtTurnID:      targetID,
	})
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}

	// 1. New session_id must differ from source.
	if newSID == sourceSession {
		t.Errorf("ForkSession returned source session_id unchanged: %q", newSID)
	}
	if newSID == "" {
		t.Errorf("ForkSession returned empty new_session_id")
	}

	// 2. Returned slice has exactly 3 clones.
	if len(newTurns) != 3 {
		t.Fatalf("ForkSession returned %d turns, want 3 (prefix root..t3)", len(newTurns))
	}

	// 3. Clones have turn_seq 1..3 (restarts in the new session).
	for i, tn := range newTurns {
		wantSeq := i + 1
		if tn.TurnSeq != wantSeq {
			t.Errorf("clone[%d].TurnSeq = %d, want %d", i, tn.TurnSeq, wantSeq)
		}
	}

	// 4. Clones carry byte-identical content_json to source t1..t3.
	for i := 0; i < 3; i++ {
		if !bytes.Equal(newTurns[i].ContentJSON, contents[i]) {
			t.Errorf("clone[%d].ContentJSON = %q, want %q (BDD-S-006.a byte-identical)",
				i, newTurns[i].ContentJSON, contents[i])
		}
	}

	// 5. Each clone has a fresh ULID distinct from the source ids.
	for i, clone := range newTurns {
		if clone.ID == ids[i] {
			t.Errorf("clone[%d].ID = %q (same as source[%d]); ULIDs must be independent", i, clone.ID, i)
		}
		if clone.ID == "" {
			t.Errorf("clone[%d].ID is empty", i)
		}
	}

	// 6. The new root turn has forked_from metadata (REQ-006).
	rootMeta := newTurns[0].Metadata
	if rootMeta == nil {
		t.Fatalf("new root turn has nil metadata; want forked_from_* set")
	}
	if got, _ := rootMeta["forked_from_session_id"].(string); got != sourceSession {
		t.Errorf("root metadata.forked_from_session_id = %q, want %q",
			got, sourceSession)
	}
	if got, _ := rootMeta["forked_from_turn_id"].(string); got != targetID {
		t.Errorf("root metadata.forked_from_turn_id = %q, want %q",
			got, targetID)
	}

	// 7. Subsequent clones parent into the previous clone (chain inside
	// the new session is linear).
	for i := 1; i < len(newTurns); i++ {
		wantParent := newTurns[i-1].ID
		if newTurns[i].ParentTurnID == nil || *newTurns[i].ParentTurnID != wantParent {
			got := "<nil>"
			if newTurns[i].ParentTurnID != nil {
				got = *newTurns[i].ParentTurnID
			}
			t.Errorf("clone[%d].ParentTurnID = %s, want %q (chain inside new session)",
				i, got, wantParent)
		}
	}

	// 8. The new root turn has parent_turn_id = NULL (it's the root of
	// a new tree, regardless of what the source's root looked like).
	if newTurns[0].ParentTurnID != nil {
		t.Errorf("new root has ParentTurnID = %v, want nil", *newTurns[0].ParentTurnID)
	}

	// 9. Source session is unchanged.
	var postCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sourceSession,
	).Scan(&postCount); err != nil {
		t.Fatalf("post-count source: %v", err)
	}
	if postCount != preCount {
		t.Errorf("source session row count changed: pre=%d, post=%d (REQ-006 non-destructive)",
			preCount, postCount)
	}

	// 10. Clones are persisted and Listable from the new session.
	listed, err := s.ListTurns(ctx, newSID, ListTurnsOpts{})
	if err != nil {
		t.Fatalf("ListTurns on new session: %v", err)
	}
	if len(listed) != 3 {
		t.Errorf("ListTurns(new session) returned %d turns, want 3", len(listed))
	}
	for i, tn := range listed {
		if tn.TurnSeq != i+1 {
			t.Errorf("listed[%d].TurnSeq = %d, want %d", i, tn.TurnSeq, i+1)
		}
		if !bytes.Equal(tn.ContentJSON, contents[i]) {
			t.Errorf("listed[%d].ContentJSON mismatch after persistence", i)
		}
	}
}

// TestForkSession_CrossProject_Rejected covers BDD-S-006.b / BDD-S-012.a / §3.5:
// the caller is in project proj-Y, the source turn lives in project proj-X
// (locked-in decision Q5). Fork must fail with ErrCrossProjectFork, and
// zero rows must be written.
func TestForkSession_CrossProject_Rejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Seed turn tA in project proj-X.
	tA, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID:   "src-cross",
		Project:     "proj-X",
		Role:        "user",
		ContentJSON: []byte(`[{"type":"text","text":"tA"}]`),
	})
	if err != nil {
		t.Fatalf("seed tA: %v", err)
	}

	// Count rows before — fork must write none.
	var preCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM session_turns`).Scan(&preCount); err != nil {
		t.Fatalf("pre-count: %v", err)
	}

	// Fork from project proj-Y targeting a turn that lives in proj-X.
	newSID, newTurns, err := s.ForkSession(ctx, ForkSessionParams{
		FromSessionID: "src-cross",
		FromProject:   "proj-Y",
		AtTurnID:      tA.ID,
	})

	if err == nil {
		t.Fatalf("ForkSession accepted cross-project fork; want ErrCrossProjectFork")
	}
	if !errors.Is(err, ErrCrossProjectFork) {
		t.Errorf("err = %v, want ErrCrossProjectFork (REQ-012)", err)
	}
	if newSID != "" || newTurns != nil {
		t.Errorf("on cross-project reject: newSID=%q newTurns=%v, want zero values", newSID, newTurns)
	}

	// Zero rows written.
	var postCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM session_turns`).Scan(&postCount); err != nil {
		t.Fatalf("post-count: %v", err)
	}
	if postCount != preCount {
		t.Errorf("cross-project reject wrote rows: pre=%d, post=%d (REQ-012 must write zero rows)",
			preCount, postCount)
	}
}

// TestForkSession_TargetNotFound covers the ErrTargetTurnNotFound path:
// a missing at_turn_id must surface as that sentinel, not a generic SQL
// error or a panic.
func TestForkSession_TargetNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, _, err := s.ForkSession(ctx, ForkSessionParams{
		FromSessionID: "src-x",
		FromProject:   "proj-X",
		AtTurnID:      "01NONEXISTENT000000000000",
	})
	if !errors.Is(err, ErrTargetTurnNotFound) {
		t.Errorf("err = %v, want ErrTargetTurnNotFound", err)
	}
}
package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"testing"
)

// helper: seed a session with 5 linear turns and return their ids in order.
// Used by the truncate-mode test cohort to avoid repeating plumbing.
func seedLinearSessionForTruncate(t *testing.T, s *Store, sessionID, project string) [5]string {
	t.Helper()
	var ids [5]string
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		var parent *string
		if i > 0 {
			p := ids[i-1]
			parent = &p
		}
		saved, err := s.SaveTurn(ctx, SaveTurnParams{
			SessionID:    sessionID,
			Project:      project,
			Role:         "user",
			ParentTurnID: parent,
			ContentJSON:  []byte(`[{"type":"text","text":"turn-` + itoa(i+1) + `"}]`),
		})
		if err != nil {
			t.Fatalf("seed turn %d: %v", i+1, err)
		}
		ids[i] = saved.ID
	}
	return ids
}

// TestRewindSession_TruncateMode_RequiresConfirmation covers REQ-011 /
// Risk #2 / Q6: truncate mode MUST be rejected with
// ErrTruncateRequiresConfirmation when the caller has not passed an
// explicit opt-in. No rows are mutated on this path.
func TestRewindSession_TruncateMode_RequiresConfirmation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const sid = "rewind-trunc-no-confirm"
	const proj = "proj-X"
	ids := seedLinearSessionForTruncate(t, s, sid, proj)

	var preCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sid,
	).Scan(&preCount); err != nil {
		t.Fatalf("pre-count: %v", err)
	}

	_, err := s.RewindSession(ctx, RewindSessionParams{
		SessionID:       sid,
		AtTurnID:        ids[1], // turn 2
		Mode:            RewindModeTruncate,
		ConfirmTruncate: false, // explicitly NOT confirmed
		FromProject:     proj,
	})
	if !errors.Is(err, ErrTruncateRequiresConfirmation) {
		t.Fatalf("err = %v, want ErrTruncateRequiresConfirmation (REQ-011)", err)
	}

	// Zero rows mutated. This is the durable signal that the rejection
	// happened before any UPDATE — callers must trust the rejection.
	var postCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sid,
	).Scan(&postCount); err != nil {
		t.Fatalf("post-count: %v", err)
	}
	if postCount != preCount {
		t.Errorf("truncate-without-confirm mutated rows: pre=%d post=%d", preCount, postCount)
	}
}

// TestRewindSession_TruncateMode_SoftDeletesDescendants covers the
// happy-path of truncate mode: when the caller passes ConfirmTruncate=true,
// descendants of AtTurnID MUST be soft-deleted via metadata flags while
// rows remain on disk (re-forkable). The target turn itself is NOT
// soft-deleted (the user can still see / reference turn N after the
// truncate — this is the "kept prefix" semantics).
func TestRewindSession_TruncateMode_SoftDeletesDescendants(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const sid = "rewind-trunc-soft"
	const proj = "proj-X"
	ids := seedLinearSessionForTruncate(t, s, sid, proj)
	// Truncate at turn 3 (ids[2]). Descendants: ids[3], ids[4] (turns 4, 5).
	target := ids[2]

	result, err := s.RewindSession(ctx, RewindSessionParams{
		SessionID:       sid,
		AtTurnID:        target,
		Mode:            RewindModeTruncate,
		ConfirmTruncate: true,
		FromProject:     proj,
	})
	if err != nil {
		t.Fatalf("RewindSession truncate-confirm: %v", err)
	}

	// SoftDeletedCount reports the number of descendants soft-deleted.
	if result.SoftDeletedCount != 2 {
		t.Errorf("SoftDeletedCount = %d, want 2 (turns 4..5)", result.SoftDeletedCount)
	}
	if result.NewSessionID != "" {
		t.Errorf("truncate mode must NOT produce a NewSessionID; got %q", result.NewSessionID)
	}

	// Row count is unchanged (soft-delete, not hard-delete).
	var rowCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sid,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 5 {
		t.Fatalf("truncate-soft-delete removed rows: count=%d, want 5", rowCount)
	}

	// Descendants (turns 4..5) MUST carry metadata.truncated_at_turn_id
	// and metadata.truncated_from_session_id. We probe each descendant
	// row directly so the test fails on either missing key.
	wantFlag := target
	wantFromSession := sid
	for _, descendantID := range []string{ids[3], ids[4]} {
		var metaRaw string
		if err := s.db.QueryRow(
			`SELECT metadata_json FROM session_turns WHERE id = ?`, descendantID,
		).Scan(&metaRaw); err != nil {
			t.Fatalf("read metadata for %s: %v", descendantID, err)
		}
		if metaRaw == "" {
			t.Errorf("descendant %s has no metadata_json; want truncated markers", descendantID)
			continue
		}
		if !strings.Contains(metaRaw, `"truncated_at_turn_id":"`+wantFlag+`"`) {
			t.Errorf("descendant %s metadata = %q; want truncated_at_turn_id=%s",
				descendantID, metaRaw, wantFlag)
		}
		if !strings.Contains(metaRaw, `"truncated_from_session_id":"`+wantFromSession+`"`) {
			t.Errorf("descendant %s metadata = %q; want truncated_from_session_id=%s",
				descendantID, metaRaw, wantFromSession)
		}
		if !strings.Contains(metaRaw, `"truncated_at":`) {
			t.Errorf("descendant %s metadata = %q; want truncated_at=<unix_ms>", descendantID, metaRaw)
		}
	}

	// The target turn itself MUST NOT carry the truncated markers — the
	// user can still reference turn 3 after the truncate (this is the
	// "kept prefix" semantics). Descendants only.
	var targetMetaRaw sql.NullString
	if err := s.db.QueryRow(
		`SELECT metadata_json FROM session_turns WHERE id = ?`, target,
	).Scan(&targetMetaRaw); err != nil {
		t.Fatalf("read target metadata: %v", err)
	}
	targetRaw := ""
	if targetMetaRaw.Valid {
		targetRaw = targetMetaRaw.String
	}
	if strings.Contains(targetRaw, "truncated_at_turn_id") {
		t.Errorf("target turn %s metadata = %q; must NOT carry truncated markers",
			target, targetRaw)
	}
	if strings.Contains(targetRaw, "truncated_from_session_id") {
		t.Errorf("target turn %s metadata = %q; must NOT carry truncated markers",
			target, targetRaw)
	}

	// Kept prefix (turns 1..3) MUST remain unchanged: their content_json
	// and role match what we seeded.
	listed, err := s.ListTurns(ctx, sid, ListTurnsOpts{})
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("ListTurns returned %d turns, want 3 (kept prefix turns 1..3)", len(listed))
	}
	for i := 0; i < 3; i++ {
		if listed[i].ID != ids[i] {
			t.Errorf("kept prefix turn[%d].id = %s, want %s (kept prefix MUST be untouched)",
				i, listed[i].ID, ids[i])
		}
		if !bytes.Contains(listed[i].ContentJSON, []byte("turn-"+itoa(i+1))) {
			t.Errorf("kept prefix turn[%d].content_json = %q, want turn-%d marker",
				i, listed[i].ContentJSON, i+1)
		}
	}
}

// TestRewindSession_TruncateMode_AuditLog covers the lock-in mitigation
// for Risk #2: every successful truncate MUST emit an audit log line
// that includes the session_id, the target turn_id, an agent_name (when
// present), and a Unix-ms timestamp. The audit line is the durable
// signal that lets ops answer "who truncated what, when?"
func TestRewindSession_TruncateMode_AuditLog(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const sid = "rewind-trunc-audit"
	const proj = "proj-X"
	ids := seedLinearSessionForTruncate(t, s, sid, proj)

	// Capture log output via the established pattern in relations_test.go.
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	agent := "agent:audit-test"
	if _, err := s.RewindSession(ctx, RewindSessionParams{
		SessionID:       sid,
		AtTurnID:        ids[1],
		Mode:            RewindModeTruncate,
		ConfirmTruncate: true,
		FromProject:     proj,
		AgentName:       &agent,
	}); err != nil {
		t.Fatalf("RewindSession truncate: %v", err)
	}

	logged := buf.String()
	requiredSubstrings := []string{
		"audit",                       // log line MUST be tagged as audit
		"session_id=" + sid,           // session_id field
		"at_turn_id=" + ids[1],        // target turn_id field
		"agent_name=" + agent,         // agent_name field
		"unix_ms=",                    // timestamp field name
	}
	for _, want := range requiredSubstrings {
		if !strings.Contains(logged, want) {
			t.Errorf("audit log missing %q; got:\n%s", want, logged)
		}
	}

	// The audit log line MUST be a single line (parseable by ops
	// log-collectors that key on logfmt).
	trimmed := strings.TrimSpace(logged)
	if strings.Contains(trimmed, "\n") {
		t.Errorf("audit log emitted multiple lines; expected single line: %q", trimmed)
	}
}

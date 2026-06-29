package store

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"testing"
)

// TestProperty_ForkPreservesTreeShape is the PR2 hardening for REQ-006.
//
// Invariants asserted over many random forests of inserts:
//
//  1. The cloned session has turn_seq == 1..N where N == prefix length,
//     strictly monotonic ascending.
//  2. content_json of every clone is byte-equal to its source turn's
//     content_json.
//  3. role is byte-equal.
//  4. The new session has EXACTLY N turns — no more, no fewer (the
//     prefix walk is bounded and the clone INSERT batch is atomic).
//  5. The source session is unchanged: row count and per-row content
//     are identical to pre-fork state.
//
// We construct a "forest" by inserting random-length linear chains
// across several sessions, then forking each at a randomly chosen
// midpoint. The chains are linear (no branching inside a single
// session) because branching mid-session requires multiple forks and
// is more thoroughly exercised in TestSessionTurnRepository_SaveAndList_SubtreeFilter
// already; the property here is "fork produces a clean prefix clone",
// not "fork handles branching topology".
func TestProperty_ForkPreservesTreeShape(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const (
		// Smaller than the 1000-forest target in TestProperty_CycleDetectionTerminates
		// because each forest here requires more rows and more
		// verification work. 30 forests × 30 turns is still 900 random
		// SaveTurn calls + 30 Forks, which is plenty to catch shape bugs.
		forestRuns  = 30
		maxTurnsRun = 30
		project     = "prop-fork"
	)

	rng := rand.New(rand.NewSource(20260629)) // fixed seed → reproducible

	for run := 0; run < forestRuns; run++ {
		sid := fmt.Sprintf("prop-fork-s-%d", run)
		chainLen := 1 + rng.Intn(maxTurnsRun) // 1..maxTurnsRun inclusive

		// Seed a linear chain of chainLen turns with content that's
		// unique per turn (so byte-identical checks can't pass by
		// accident on a duplicate-content chain).
		contents := make([][]byte, chainLen)
		var ids []string
		for i := 0; i < chainLen; i++ {
			contents[i] = []byte(fmt.Sprintf(`[{"type":"text","text":"run=%d turn=%d payload=%x"}]`,
				run, i, rng.Int63()))
			var parent *string
			if i > 0 {
				p := ids[len(ids)-1]
				parent = &p
			}
			saved, err := s.SaveTurn(ctx, SaveTurnParams{
				SessionID:    sid,
				Project:      project,
				Role:         "user",
				ParentTurnID: parent,
				ContentJSON:  contents[i],
			})
			if err != nil {
				t.Fatalf("run %d turn %d: SaveTurn: %v", run, i, err)
			}
			ids = append(ids, saved.ID)
		}

		// Pick a random fork point. Anything from 1..chainLen (1 is
		// allowed — forking at the very root is a degenerate but legal
		// case).
		forkIdx := rng.Intn(chainLen)
		targetID := ids[forkIdx]

		// Snapshot source state for the no-mutation assertion.
		var preCount int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sid,
		).Scan(&preCount); err != nil {
			t.Fatalf("run %d pre-count: %v", run, err)
		}
		preContents, err := fetchSessionContents(ctx, s, sid)
		if err != nil {
			t.Fatalf("run %d pre-fetch: %v", run, err)
		}

		// Fork.
		newSID, newTurns, err := s.ForkSession(ctx, ForkSessionParams{
			FromSessionID: sid,
			FromProject:   project,
			AtTurnID:      targetID,
		})
		if err != nil {
			t.Fatalf("run %d fork at idx %d: %v", run, forkIdx, err)
		}

		// 1. Clone count == prefix length == forkIdx+1.
		wantClones := forkIdx + 1
		if len(newTurns) != wantClones {
			t.Errorf("run %d forkIdx=%d: got %d clones, want %d",
				run, forkIdx, len(newTurns), wantClones)
			continue
		}

		// 2. Strictly monotonic turn_seq 1..wantClones.
		for i, tn := range newTurns {
			wantSeq := i + 1
			if tn.TurnSeq != wantSeq {
				t.Errorf("run %d clone[%d].TurnSeq = %d, want %d",
					run, i, tn.TurnSeq, wantSeq)
			}
			if tn.SessionID != newSID {
				t.Errorf("run %d clone[%d].SessionID = %q, want %q",
					run, i, tn.SessionID, newSID)
			}
			if tn.Project != project {
				t.Errorf("run %d clone[%d].Project = %q, want %q",
					run, i, tn.Project, project)
			}
		}

		// 3. Byte-identical content and role for every clone.
		for i := 0; i < wantClones; i++ {
			if !bytes.Equal(newTurns[i].ContentJSON, contents[i]) {
				t.Errorf("run %d clone[%d].ContentJSON differs from source[%d]",
					run, i, i)
			}
			if newTurns[i].Role != "user" {
				t.Errorf("run %d clone[%d].Role = %q, want %q",
					run, i, newTurns[i].Role, "user")
			}
		}

		// 4. Persisted state matches the returned slice.
		persisted, err := s.ListTurns(ctx, newSID, ListTurnsOpts{})
		if err != nil {
			t.Fatalf("run %d ListTurns new: %v", run, err)
		}
		if len(persisted) != wantClones {
			t.Errorf("run %d persisted clones = %d, want %d",
				run, len(persisted), wantClones)
		}
		for i := 0; i < wantClones && i < len(persisted); i++ {
			if !bytes.Equal(persisted[i].ContentJSON, contents[i]) {
				t.Errorf("run %d persisted[%d].ContentJSON differs from source[%d]",
					run, i, i)
			}
		}

		// 5. Source is unchanged: row count and per-row content identical.
		var postCount int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sid,
		).Scan(&postCount); err != nil {
			t.Fatalf("run %d post-count: %v", run, err)
		}
		if postCount != preCount {
			t.Errorf("run %d source row count changed: pre=%d post=%d (REQ-006 non-destructive)",
				run, preCount, postCount)
		}
		postContents, err := fetchSessionContents(ctx, s, sid)
		if err != nil {
			t.Fatalf("run %d post-fetch: %v", run, err)
		}
		if !bytesContentsEqual(preContents, postContents) {
			t.Errorf("run %d source content mutated by fork", run)
		}
	}
}

// fetchSessionContents returns the content_json bytes of every turn in
// the session, ordered by turn_seq ASC. Used to snapshot and compare
// source-side state across the fork call.
func fetchSessionContents(ctx context.Context, s *Store, sessionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT content_json FROM session_turns WHERE session_id = ? ORDER BY turn_seq ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("fetchSessionContents query: %w", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("fetchSessionContents scan: %w", err)
		}
		out = append(out, []byte(c))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetchSessionContents rows: %w", err)
	}
	return out, nil
}

// bytesContentsEqual compares two content slices element-wise. Used in
// the property test to detect "fork wrote back to source" regressions.
func bytesContentsEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
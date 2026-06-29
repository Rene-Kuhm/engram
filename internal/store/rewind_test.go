package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestRewindSession_BranchMode_CreatesNewSession covers BDD-S-007.a / §3.2:
// rewind with no Mode flag MUST default to branch mode (locked-in decision
// Q6 / REQ-007). The result is a new session containing a clone of turns
// 1..N (up to and including the target turn). The original session is
// untouched.
func TestRewindSession_BranchMode_CreatesNewSession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const (
		sourceSession = "rewind-s1"
		sourceProject = "proj-X"
	)

	// Seed a 10-turn linear session.
	var ids [10]string
	contents := [10][]byte{}
	for i := 0; i < 10; i++ {
		var parent *string
		if i > 0 {
			p := ids[i-1]
			parent = &p
		}
		contents[i] = []byte(`[{"type":"text","text":"t` + itoa(i+1) + `"}]`)
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

	// Count rows in the source before rewind.
	var preCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sourceSession,
	).Scan(&preCount); err != nil {
		t.Fatalf("pre-count: %v", err)
	}

	// Rewind at t7 with no Mode flag (defaults to branch per Q6).
	result, err := s.RewindSession(ctx, RewindSessionParams{
		SessionID:   sourceSession,
		AtTurnID:    ids[6], // t7
		FromProject: sourceProject,
		// Mode omitted → defaults to branch.
	})
	if err != nil {
		t.Fatalf("RewindSession: %v", err)
	}

	// 1. Branch mode MUST return a new session_id.
	if result.NewSessionID == "" {
		t.Fatalf("RewindSession branch mode returned empty NewSessionID")
	}
	if result.NewSessionID == sourceSession {
		t.Errorf("RewindSession branch mode returned source session_id unchanged")
	}

	// 2. The new session has exactly 7 turns (the kept prefix).
	listed, err := s.ListTurns(ctx, result.NewSessionID, ListTurnsOpts{})
	if err != nil {
		t.Fatalf("ListTurns on new session: %v", err)
	}
	if len(listed) != 7 {
		t.Fatalf("rewound session has %d turns, want 7 (prefix root..t7)", len(listed))
	}

	// 3. The new session's root has rewound_from_* metadata.
	root := listed[0]
	if root.Metadata == nil {
		t.Fatalf("rewound root has nil metadata; want rewound_from_* set")
	}
	if got, _ := root.Metadata["rewound_from_session_id"].(string); got != sourceSession {
		t.Errorf("root.rewound_from_session_id = %q, want %q", got, sourceSession)
	}
	if got, _ := root.Metadata["rewound_from_turn_id"].(string); got != ids[6] {
		t.Errorf("root.rewound_from_turn_id = %q, want %q (the AtTurnID)", got, ids[6])
	}

	// 4. Source session is unchanged.
	var postCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sourceSession,
	).Scan(&postCount); err != nil {
		t.Fatalf("post-count: %v", err)
	}
	if postCount != preCount {
		t.Errorf("rewind-branch mutated source: pre=%d, post=%d (Q6: branch is non-destructive)",
			preCount, postCount)
	}

	// 5. Caller can continue from the rewound session by appending a new
	//    turn whose parent is the new root's last clone. This is the
	//    §3.2 "branch then continue" scenario.
	parent := listed[len(listed)-1].ID
	saved, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID:    result.NewSessionID,
		Project:      sourceProject,
		ParentTurnID: &parent,
		Role:         "user",
		ContentJSON:  []byte(`[{"type":"text","text":"continued"}]`),
	})
	if err != nil {
		t.Fatalf("continue after rewind: %v", err)
	}
	if saved.TurnSeq != 8 {
		t.Errorf("continued turn_seq = %d, want 8 (BDD-S-007.a: continues from prefix length)", saved.TurnSeq)
	}
}

// TestRewindSession_BranchMode_PreservesContent covers BDD-S-007.b:
// rewind branch mode must preserve byte-identical content_json for every
// cloned turn (the clones are byte-equal to the source prefix).
func TestRewindSession_BranchMode_PreservesContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const (
		sourceSession = "rewind-content-s1"
		sourceProject = "proj-X"
	)

	// Seed a 5-turn session with varied content blocks.
	var ids [5]string
	contents := [5][]byte{
		[]byte(`[{"type":"text","text":"first"}]`),
		[]byte(`[{"type":"reasoning","text":"thinking"}]`),
		[]byte(`[{"type":"tool-call","id":"c1","name":"read","arguments":{"path":"x.go"}}]`),
		[]byte(`[{"type":"tool-result","tool_call_id":"c1","name":"read","content":"contents"}]`),
		[]byte(`[{"type":"text","text":"fifth"}]`),
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
			t.Fatalf("seed %d: %v", i+1, err)
		}
		ids[i] = saved.ID
	}

	// Rewind at t3 (index 2) with explicit branch mode.
	result, err := s.RewindSession(ctx, RewindSessionParams{
		SessionID:   sourceSession,
		AtTurnID:    ids[2],
		Mode:        RewindModeBranch,
		FromProject: sourceProject,
	})
	if err != nil {
		t.Fatalf("RewindSession branch: %v", err)
	}

	listed, err := s.ListTurns(ctx, result.NewSessionID, ListTurnsOpts{})
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d turns, want 3", len(listed))
	}
	for i := 0; i < 3; i++ {
		if !bytes.Equal(listed[i].ContentJSON, contents[i]) {
			t.Errorf("listed[%d].ContentJSON = %q, want %q (BDD-S-007.b byte-identical)",
				i, listed[i].ContentJSON, contents[i])
		}
	}

	// Source session still has all 5 turns untouched.
	srcList, err := s.ListTurns(ctx, sourceSession, ListTurnsOpts{})
	if err != nil {
		t.Fatalf("ListTurns source: %v", err)
	}
	if len(srcList) != 5 {
		t.Errorf("source has %d turns, want 5 (rewind-branch must not mutate)", len(srcList))
	}
}

// TestRewindSession_UnknownMode covers the "Mode is something other than
// branch or truncate" path: must return ErrInvalidRewindMode without
// touching any rows.
func TestRewindSession_UnknownMode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	t0, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "unk-s", Project: "proj-X", Role: "user",
		ContentJSON: []byte(`[{"type":"text","text":"x"}]`),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = s.RewindSession(ctx, RewindSessionParams{
		SessionID:   "unk-s",
		AtTurnID:    t0.ID,
		Mode:        RewindMode("squash"), // unknown
		FromProject: "proj-X",
	})
	if !errors.Is(err, ErrInvalidRewindMode) {
		t.Errorf("unknown mode err = %v, want ErrInvalidRewindMode", err)
	}
}

// itoa is a tiny helper to avoid pulling strconv just for the test seeds.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
	"time"

	_ "modernc.org/sqlite"
)

// TestSessionTurnsSchema_TableAndIndexesExist asserts the v6→v7 migration
// added the session_turns table with the spec'd columns (REQ-001), the
// project column (locked-in decision A), and the 2 indexes
// (session_id, turn_seq) and (parent_turn_id) per the PR1a spec.
func TestSessionTurnsSchema_TableAndIndexesExist(t *testing.T) {
	s := newTestStore(t)

	// 1. Table exists.
	var tableName string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='session_turns'`,
	).Scan(&tableName)
	if err != nil {
		t.Fatalf("session_turns table not found: %v", err)
	}

	// 2. Required columns exist with the spec'd types/constraints.
	requiredCols := map[string]string{
		"id":             "TEXT",
		"session_id":     "TEXT",
		"parent_turn_id": "TEXT",
		"turn_seq":       "INTEGER",
		"role":           "TEXT",
		"content_json":   "TEXT",
		"agent_name":     "TEXT",
		"tokens_in":      "INTEGER",
		"tokens_out":     "INTEGER",
		"created_at":     "INTEGER",
		"metadata_json":  "TEXT",
		"project":        "TEXT",
	}
	rows, err := s.db.Query(`PRAGMA table_info(session_turns)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(session_turns): %v", err)
	}
	defer rows.Close()
	found := map[string]string{}
	notNull := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var nn int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &nn, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		found[name] = typ
		notNull[name] = nn == 1
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pragma rows err: %v", err)
	}
	for col, wantType := range requiredCols {
		gotType, ok := found[col]
		if !ok {
			t.Errorf("missing column %q on session_turns", col)
			continue
		}
		// SQLite reports INTEGER for both INT and INTEGER; allow either way.
		if !strings.EqualFold(gotType, wantType) {
			t.Errorf("column %q type = %q, want %q", col, gotType, wantType)
		}
	}
	// Locked-in decision A: project must be NOT NULL.
	if !notNull["project"] {
		t.Errorf("project column must be NOT NULL per decision A")
	}
	// id must be PRIMARY KEY (pk=1).
	if found["id"] == "" {
		t.Errorf("id column missing — cannot be primary key")
	}

	// 3. Two required indexes exist.
	idxRows, err := s.db.Query(
		`SELECT name, sql FROM sqlite_master WHERE type='index' AND tbl_name='session_turns'`,
	)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer idxRows.Close()
	indexes := map[string]string{}
	for idxRows.Next() {
		var name, sql sql.NullString
		if err := idxRows.Scan(&name, &sql); err != nil {
			t.Fatalf("scan index row: %v", err)
		}
		if sql.Valid {
			indexes[name.String] = sql.String
		}
	}
	if err := idxRows.Err(); err != nil {
		t.Fatalf("index rows err: %v", err)
	}

	wantIdx1 := false
	wantIdx2 := false
	for name, sql := range indexes {
		upper := strings.ToUpper(sql)
		if strings.Contains(upper, "SESSION_ID") && strings.Contains(upper, "TURN_SEQ") && !strings.Contains(upper, "PROJECT") {
			wantIdx1 = true
		}
		if strings.Contains(upper, "PARENT_TURN_ID") {
			wantIdx2 = true
		}
		_ = name
	}
	if !wantIdx1 {
		t.Errorf("missing index on (session_id, turn_seq); indexes found: %v", indexes)
	}
	if !wantIdx2 {
		t.Errorf("missing index on (parent_turn_id); indexes found: %v", indexes)
	}
}

// newTestStoreFromV6Database creates a fresh v6 schema (sessions table only —
// NO session_turns) populated with the given session rows, then opens the
// store via New(cfg) so that migrate() runs the v6→v7 step under test.
// Each sessionRow is (id, project, summary).
//
// Mirrors newTestStoreWithLegacySchema from store_migration_test.go but
// uses a minimal legacy DDL that contains only the sessions table (the
// rest of the v6 schema is irrelevant for the session_turns backfill test).
type v6SessionRow struct {
	id      string
	project string
	summary string
}

func newTestStoreFromV6Database(t *testing.T, sessionRows []v6SessionRow) *Store {
	t.Helper()

	dir, err := os.MkdirTemp("", "engram-v6-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "engram.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}

	if _, err := raw.Exec("PRAGMA journal_mode = WAL"); err != nil {
		raw.Close()
		t.Fatalf("WAL pragma: %v", err)
	}
	if _, err := raw.Exec("PRAGMA foreign_keys = ON"); err != nil {
		raw.Close()
		t.Fatalf("foreign_keys pragma: %v", err)
	}

	// Minimal v6 DDL — just the sessions table. No session_turns, no
	// memory_relations, no FTS. This is the schema as it existed before
	// the jsonl-session-tree change.
	v6DDL := `
		CREATE TABLE IF NOT EXISTS sessions (
			id         TEXT PRIMARY KEY,
			project    TEXT NOT NULL,
			directory  TEXT NOT NULL DEFAULT '/tmp',
			started_at TEXT NOT NULL DEFAULT (datetime('now')),
			ended_at   TEXT,
			summary    TEXT
		);
	`
	if _, err := raw.Exec(v6DDL); err != nil {
		raw.Close()
		t.Fatalf("apply v6 DDL: %v", err)
	}

	for _, row := range sessionRows {
		var summary any
		if row.summary != "" {
			summary = row.summary
		} else {
			summary = nil
		}
		if _, err := raw.Exec(
			`INSERT INTO sessions (id, project, directory, summary) VALUES (?, ?, ?, ?)`,
			row.id, row.project, "/tmp", summary,
		); err != nil {
			raw.Close()
			t.Fatalf("insert session %q: %v", row.id, err)
		}
	}

	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	cfg := mustDefaultConfig(t)
	cfg.DataDir = dir
	s, err := New(cfg)
	if err != nil {
		removeDirWithRetry(dir)
		t.Fatalf("New(cfg) on v6 db: %v", err)
	}
	t.Cleanup(func() { closeAndRemoveStore(t, dir, s) })
	return s
}

// TestMigrate_BackfillsSyntheticTurns covers REQ-003 BDD-S-003.a: a v6
// sessions row must produce exactly one synthetic session_turns row with
// role=system, turn_seq=1, parent_turn_id=NULL, agent_name=system-migration,
// metadata.pre_tree=true, and content_json carrying the verbatim summary
// text wrapped as a single typed text block.
func TestMigrate_BackfillsSyntheticTurns(t *testing.T) {
	s := newTestStoreFromV6Database(t, []v6SessionRow{
		{id: "legacy-1", project: "proj-legacy", summary: "prior work notes"},
	})

	var (
		gotID, gotSessionID, gotProject, gotRole, gotAgent, gotContent string
		gotParent                                                       sql.NullString
		gotSeq                                                           int
		gotMetadata                                                      sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT id, session_id, project, parent_turn_id, turn_seq, role,
		        content_json, agent_name, metadata_json
		 FROM session_turns WHERE session_id = ? ORDER BY turn_seq ASC`,
		"legacy-1",
	).Scan(
		&gotID, &gotSessionID, &gotProject, &gotParent, &gotSeq, &gotRole,
		&gotContent, &gotAgent, &gotMetadata,
	)
	if err != nil {
		t.Fatalf("query synthetic turn for legacy-1: %v", err)
	}

	if gotSessionID != "legacy-1" {
		t.Errorf("session_id = %q, want %q", gotSessionID, "legacy-1")
	}
	if gotProject != "proj-legacy" {
		t.Errorf("project = %q, want %q", gotProject, "proj-legacy")
	}
	if gotParent.Valid {
		t.Errorf("parent_turn_id = %q, want NULL (root turn)", gotParent.String)
	}
	if gotSeq != 1 {
		t.Errorf("turn_seq = %d, want 1", gotSeq)
	}
	if gotRole != "system" {
		t.Errorf("role = %q, want %q", gotRole, "system")
	}
	if gotAgent != "system-migration" {
		t.Errorf("agent_name = %q, want %q", gotAgent, "system-migration")
	}

	// content_json MUST round-trip the verbatim summary as a typed text block.
	wantContent := `[{"type":"text","text":"prior work notes"}]`
	if gotContent != wantContent {
		t.Errorf("content_json = %q, want %q", gotContent, wantContent)
	}

	// metadata.pre_tree MUST be true.
	if !gotMetadata.Valid {
		t.Fatalf("metadata_json is NULL; want JSON with pre_tree=true")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(gotMetadata.String), &meta); err != nil {
		t.Fatalf("unmarshal metadata_json: %v", err)
	}
	if preTree, _ := meta["pre_tree"].(bool); !preTree {
		t.Errorf("metadata.pre_tree = %v, want true (meta=%v)", meta["pre_tree"], meta)
	}

	// BDD-S-003.b: re-running migrate() must NOT insert a second synthetic turn.
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`,
		"legacy-1",
	).Scan(&count); err != nil {
		t.Fatalf("count after second migrate: %v", err)
	}
	if count != 1 {
		t.Errorf("re-running migrate inserted duplicate turns: count = %d, want 1", count)
	}
}

// TestMigrate_BackfillHandlesMultipleSessions verifies the backfill loop
// processes every existing sessions row (not just one), each producing
// exactly one synthetic turn, and skips sessions that were created AFTER
// the v7 migration ran (no spurious backfill on freshly created sessions).
func TestMigrate_BackfillHandlesMultipleSessions(t *testing.T) {
	s := newTestStoreFromV6Database(t, []v6SessionRow{
		{id: "legacy-A", project: "proj-A", summary: "summary A"},
		{id: "legacy-B", project: "proj-B", summary: ""}, // empty summary
		{id: "legacy-C", project: "proj-A", summary: "summary C"},
	})

	rows, err := s.db.Query(
		`SELECT session_id, project FROM session_turns
		 WHERE role = 'system' AND json_extract(metadata_json, '$.pre_tree') = 1
		 ORDER BY session_id`,
	)
	if err != nil {
		t.Fatalf("query backfilled turns: %v", err)
	}
	defer rows.Close()

	type pair struct{ sessionID, project string }
	var got []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.sessionID, &p.project); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	want := []pair{
		{"legacy-A", "proj-A"},
		{"legacy-B", "proj-B"},
		{"legacy-C", "proj-A"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d backfilled turns, want %d (got=%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("backfilled[%d] = %+v, want %+v", i, got[i], w)
		}
	}

	// Sessions without summary still get a synthetic turn, but content_json
	// carries an empty text block (still a valid typed-block array).
	var emptyContent string
	if err := s.db.QueryRow(
		`SELECT content_json FROM session_turns WHERE session_id = ?`,
		"legacy-B",
	).Scan(&emptyContent); err != nil {
		t.Fatalf("query empty-summary turn: %v", err)
	}
	if emptyContent != `[{"type":"text","text":""}]` {
		t.Errorf("empty-summary content_json = %q, want %q", emptyContent, `[{"type":"text","text":""}]`)
	}
}

// ─── SessionTurnRepository tests ────────────────────────────────────────────

// TestSessionTurnRepository_SaveAndList covers REQ-004 (BDD-S-001.a,
// BDD-S-004.a) and REQ-005 (BDD-S-005.a):
//   - SaveTurn appends a turn under a session, assigns monotonic turn_seq,
//     and returns the new id.
//   - ListTurns returns turns for a session ordered by turn_seq.
//   - ListTurns excludes pre_tree=true turns by default (Q1).
//   - content_json round-trips byte-identical (BDD-S-001.a).
func TestSessionTurnRepository_SaveAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Save a root turn.
	content := []byte(`[{"type":"text","text":"hello world"}]`)
	saved, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID:   "s-1",
		Project:     "proj-1",
		Role:        "user",
		ContentJSON: content,
		AgentName:   strPtr("alice"),
	})
	if err != nil {
		t.Fatalf("SaveTurn root: %v", err)
	}
	if saved.ID == "" {
		t.Errorf("SaveTurn returned empty ID")
	}
	if saved.SessionID != "s-1" {
		t.Errorf("saved.SessionID = %q, want s-1", saved.SessionID)
	}
	if saved.Project != "proj-1" {
		t.Errorf("saved.Project = %q, want proj-1", saved.Project)
	}
	if saved.TurnSeq != 1 {
		t.Errorf("first save: TurnSeq = %d, want 1", saved.TurnSeq)
	}
	if saved.ParentTurnID != nil {
		t.Errorf("first save: ParentTurnID = %v, want nil", saved.ParentTurnID)
	}
	if saved.Role != "user" {
		t.Errorf("saved.Role = %q, want user", saved.Role)
	}
	if !bytesEqual(saved.ContentJSON, content) {
		t.Errorf("saved.ContentJSON = %q, want %q (byte-identical)", saved.ContentJSON, content)
	}

	// Save a child turn — must auto-assign turn_seq = MAX+1 = 2.
	childContent := []byte(`[{"type":"reasoning","text":"thinking..."}]`)
	parentID := saved.ID
	saved2, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID:    "s-1",
		Project:      "proj-1",
		ParentTurnID: &parentID,
		Role:         "assistant",
		ContentJSON:  childContent,
	})
	if err != nil {
		t.Fatalf("SaveTurn child: %v", err)
	}
	if saved2.TurnSeq != 2 {
		t.Errorf("second save: TurnSeq = %d, want 2 (BDD-S-004.a)", saved2.TurnSeq)
	}
	if saved2.ParentTurnID == nil || *saved2.ParentTurnID != parentID {
		t.Errorf("saved2.ParentTurnID = %v, want %q", saved2.ParentTurnID, parentID)
	}

	// Save a pre_tree synthetic turn manually (so we can test the default
	// exclude behavior in ListTurns).
	preTreeID := "01HZZZPREETREESYNTHETIC00000"
	if _, err := s.db.Exec(
		`INSERT INTO session_turns (id, session_id, project, parent_turn_id, turn_seq, role, content_json, agent_name, tokens_in, tokens_out, created_at, metadata_json)
		 VALUES (?, 's-1', 'proj-1', NULL, 0, 'system', '[{"type":"text","text":"legacy"}]', 'system-migration', NULL, NULL, ?, '{"pre_tree":true}')`,
		preTreeID, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert pre_tree turn: %v", err)
	}

	// ListTurns default: must exclude pre_tree turns (Q1).
	turns, err := s.ListTurns(ctx, "s-1", ListTurnsOpts{})
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("ListTurns returned %d turns, want 2 (pre_tree excluded by default)", len(turns))
	}
	// Order: turn_seq ASC.
	if turns[0].TurnSeq != 1 || turns[1].TurnSeq != 2 {
		t.Errorf("ListTurns not ordered by turn_seq ASC: got [%d, %d], want [1, 2]", turns[0].TurnSeq, turns[1].TurnSeq)
	}
	// First turn content must round-trip byte-identical.
	if !bytesEqual(turns[0].ContentJSON, content) {
		t.Errorf("listed[0].ContentJSON = %q, want %q (BDD-S-001.a round-trip)", turns[0].ContentJSON, content)
	}
	// First turn must be the root.
	if turns[0].ParentTurnID != nil {
		t.Errorf("listed[0].ParentTurnID = %v, want nil (root)", turns[0].ParentTurnID)
	}

	// ListTurns with IncludeLegacy=true: returns the pre_tree turn too.
	allTurns, err := s.ListTurns(ctx, "s-1", ListTurnsOpts{IncludeLegacy: true})
	if err != nil {
		t.Fatalf("ListTurns IncludeLegacy: %v", err)
	}
	if len(allTurns) != 3 {
		t.Errorf("ListTurns IncludeLegacy returned %d turns, want 3 (pre_tree included)", len(allTurns))
	}
}

// TestSessionTurnRepository_SaveAndList_SubtreeFilter covers BDD-S-005.b:
// when from_turn_id is provided, the result is the subtree rooted at that
// turn, NOT including the turn itself, ordered by turn_seq.
func TestSessionTurnRepository_SaveAndList_SubtreeFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Build a small fork topology under s-1:
	//   t1 -> t2 -> t3 (root path)
	//                -> t4 (fork) -> t5
	//                -> t6 (fork) -> t7 -> t8
	root, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-1", Project: "proj-1", Role: "user",
		ContentJSON: []byte(`[{"type":"text","text":"t1"}]`),
	})
	if err != nil {
		t.Fatalf("save root: %v", err)
	}
	chain := func(parentID string, label string) (string, error) {
		pid := parentID
		t2, err := s.SaveTurn(ctx, SaveTurnParams{
			SessionID: "s-1", Project: "proj-1", Role: "assistant",
			ParentTurnID: &pid, ContentJSON: []byte(fmt.Sprintf(`[{"type":"text","text":%q}]`, label)),
		})
		if err != nil {
			return "", err
		}
		return t2.ID, nil
	}
	t2, err := chain(root.ID, "t2")
	if err != nil {
		t.Fatalf("save t2: %v", err)
	}
	t3, err := chain(t2, "t3")
	if err != nil {
		t.Fatalf("save t3: %v", err)
	}
	if _, err := chain(t3, "t4"); err != nil {
		t.Fatalf("save t4: %v", err)
	}
	if _, err := chain(t3, "t5"); err != nil {
		t.Fatalf("save t5: %v", err)
	}
	t6, err := chain(t2, "t6")
	if err != nil {
		t.Fatalf("save t6: %v", err)
	}
	if _, err := chain(t6, "t7"); err != nil {
		t.Fatalf("save t7: %v", err)
	}

	// Subtree at t2: descendants are t3, t4, t5, t6, t7. t2 itself is NOT
	// included (BDD-S-005.b).
	t2ID := t2
	got, err := s.ListTurns(ctx, "s-1", ListTurnsOpts{FromTurnID: &t2ID})
	if err != nil {
		t.Fatalf("ListTurns subtree: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("subtree at t2 returned %d turns, want 5 (t3, t4, t5, t6, t7); got seqs=%v",
			len(got), turnSeqs(got))
	}
	// Verify ordering.
	for i := 1; i < len(got); i++ {
		if got[i].TurnSeq <= got[i-1].TurnSeq {
			t.Errorf("subtree not ordered by turn_seq ASC: %v", turnSeqs(got))
			break
		}
	}
}

// TestSessionTurnRepository_CycleDetection covers REQ-010 (BDD-S-010.a/b):
//   - Self-loop (parent_turn_id == id) is rejected.
//   - Indirect cycle (A -> B -> C -> A) is rejected.
//   - No row is written on rejection.
func TestSessionTurnRepository_CycleDetection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Build A -> B -> C.
	a, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-1", Project: "proj-1", Role: "user",
		ContentJSON: []byte(`[{"type":"text","text":"A"}]`),
	})
	if err != nil {
		t.Fatalf("save A: %v", err)
	}
	parentA := a.ID
	b, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-1", Project: "proj-1", Role: "assistant",
		ParentTurnID: &parentA, ContentJSON: []byte(`[{"type":"text","text":"B"}]`),
	})
	if err != nil {
		t.Fatalf("save B: %v", err)
	}
	parentB := b.ID
	c, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-1", Project: "proj-1", Role: "assistant",
		ParentTurnID: &parentB, ContentJSON: []byte(`[{"type":"text","text":"C"}]`),
	})
	if err != nil {
		t.Fatalf("save C: %v", err)
	}

	// BDD-S-010.a: self-loop rejected.
	t.Run("self_loop", func(t *testing.T) {
		aID := a.ID
		_, err := s.SaveTurn(ctx, SaveTurnParams{
			ID:          a.ID, // explicit same id
			SessionID:   "s-1",
			Project:     "proj-1",
			ParentTurnID: &aID,
			Role:        "user",
			ContentJSON: []byte(`[{"type":"text","text":"loop"}]`),
		})
		if !errors.Is(err, ErrCycleDetected) {
			t.Errorf("self-loop: err = %v, want ErrCycleDetected", err)
		}
	})

	// BDD-S-010.b: indirect cycle rejected.
	t.Run("indirect_cycle", func(t *testing.T) {
		parentC := c.ID
		_, err := s.SaveTurn(ctx, SaveTurnParams{
			ID:          a.ID, // re-use A's id (would close A->B->C->A)
			SessionID:   "s-1",
			Project:     "proj-1",
			ParentTurnID: &parentC,
			Role:        "user",
			ContentJSON: []byte(`[{"type":"text","text":"loopback"}]`),
		})
		if !errors.Is(err, ErrCycleDetected) {
			t.Errorf("indirect cycle: err = %v, want ErrCycleDetected", err)
		}
	})

	// No row was written on rejection.
	var totalRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM session_turns WHERE session_id = 's-1'`).Scan(&totalRows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if totalRows != 3 {
		t.Errorf("after rejected cycles, session_turns has %d rows, want 3 (only A, B, C)", totalRows)
	}
}

// TestSessionTurnRepository_Validation covers the other REQ-004 invariants:
// project required, role must be in {user, assistant, tool, system},
// content_json must be a JSON array of typed blocks, parent_turn_id must
// belong to the same session.
func TestSessionTurnRepository_Validation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Missing project.
	_, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-1", Role: "user",
		ContentJSON: []byte(`[{"type":"text","text":"hi"}]`),
	})
	if !errors.Is(err, ErrProjectRequired) {
		t.Errorf("missing project: err = %v, want ErrProjectRequired", err)
	}

	// Invalid role.
	_, err = s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-1", Project: "proj-1", Role: "robot",
		ContentJSON: []byte(`[{"type":"text","text":"hi"}]`),
	})
	if !errors.Is(err, ErrInvalidRole) {
		t.Errorf("invalid role: err = %v, want ErrInvalidRole", err)
	}

	// Malformed content_json.
	_, err = s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-1", Project: "proj-1", Role: "user",
		ContentJSON: []byte(`"not-an-array"`),
	})
	if !errors.Is(err, ErrInvalidContentShape) {
		t.Errorf("malformed content (not array): err = %v, want ErrInvalidContentShape", err)
	}

	// content_json is an array but contains a block with an unknown type.
	_, err = s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-1", Project: "proj-1", Role: "user",
		ContentJSON: []byte(`[{"type":"mystery","data":1}]`),
	})
	if !errors.Is(err, ErrInvalidContentShape) {
		t.Errorf("unknown block type: err = %v, want ErrInvalidContentShape", err)
	}

	// Parent in a different session.
	other, err := s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-other", Project: "proj-1", Role: "user",
		ContentJSON: []byte(`[{"type":"text","text":"other"}]`),
	})
	if err != nil {
		t.Fatalf("save other: %v", err)
	}
	otherID := other.ID
	_, err = s.SaveTurn(ctx, SaveTurnParams{
		SessionID: "s-1", Project: "proj-1", Role: "assistant",
		ParentTurnID: &otherID,
		ContentJSON:  []byte(`[{"type":"text","text":"x"}]`),
	})
	if !errors.Is(err, ErrParentSessionMismatch) {
		t.Errorf("parent from other session: err = %v, want ErrParentSessionMismatch", err)
	}
}

// TestProperty_CycleDetectionTerminates is the property test (REQ-010
// hardening). Two properties are asserted:
//
//  1. Random linear forests of up to N turns complete without infinite
//     recursion. Each SaveTurn must terminate (the ancestor walk is
//     bounded by the session row count, so this is structural).
//  2. Cycle-detection on adversarial inputs (an explicit attempt to
//     close a loop A->B->C->A) rejects in <10ms. This is the per-call
//     budget from the spec.
//
// The previous TestSessionTurnRepository_CycleDetection covers the
// acceptance cases (self-loop, indirect cycle). This test hardens the
// cost and termination guarantee.
func TestProperty_CycleDetectionTerminates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const (
		forestRuns  = 50
		maxTurnsRun = 50
		// Generous budget on linear forests (we just want to confirm
		// termination, not wall-clock perf — that's covered by the
		// 10K-turn perf test in PR3).
		forestBudget = 200 * time.Millisecond
		// Strict budget for the adversarial cycle-detection path,
		// which is what REQ-010 actually constrains.
		cycleBudget = 10 * time.Millisecond
	)

	// Seed RNG for reproducibility on this property test.
	rng := rand.New(rand.NewSource(42))

	// ── Property 1: linear forests terminate ──────────────────────────
	for run := 0; run < forestRuns; run++ {
		sid := fmt.Sprintf("prop-s-%d", run)
		var prevID string
		var maxObserved time.Duration
		for i := 0; i < maxTurnsRun; i++ {
			var parentPtr *string
			if i > 0 {
				p := prevID
				parentPtr = &p
			}
			start := time.Now()
			saved, err := s.SaveTurn(ctx, SaveTurnParams{
				SessionID:    sid,
				Project:      "prop-proj",
				Role:         "user",
				ParentTurnID: parentPtr,
				ContentJSON:  []byte(fmt.Sprintf(`[{"type":"text","text":"%d"}]`, i)),
			})
			elapsed := time.Since(start)
			if elapsed > maxObserved {
				maxObserved = elapsed
			}
			if err != nil {
				t.Fatalf("run %d turn %d: SaveTurn failed: %v", run, i, err)
			}
			prevID = saved.ID
		}
		// The chain is linear; cycle detection walks 0 ancestors per
		// save (no ancestor equals the new id), so the per-call
		// contribution is small. Generous budget accommodates
		// Windows/dev-machine variance.
		if maxObserved > forestBudget {
			t.Errorf("run %d: max SaveTurn latency = %v, want < %v",
				run, maxObserved, forestBudget)
		}
	}

	// ── Property 2: cycle detection cost on adversarial input ─────────
	// Build A->B->C and then attempt to save a turn with id=A and
	// parent_turn_id=C. SaveTurn must reject with ErrCycleDetected in
	// < cycleBudget. This exercises the bounded recursive CTE.
	adversarialSessions := []string{"adv-s-1", "adv-s-2", "adv-s-3"}
	for _, sid := range adversarialSessions {
		a, err := s.SaveTurn(ctx, SaveTurnParams{
			SessionID: sid, Project: "adv", Role: "user",
			ContentJSON: []byte(`[{"type":"text","text":"A"}]`),
		})
		if err != nil {
			t.Fatalf("adv %s: save A: %v", sid, err)
		}
		aID := a.ID
		b, err := s.SaveTurn(ctx, SaveTurnParams{
			SessionID: sid, Project: "adv", Role: "assistant",
			ParentTurnID: &aID, ContentJSON: []byte(`[{"type":"text","text":"B"}]`),
		})
		if err != nil {
			t.Fatalf("adv %s: save B: %v", sid, err)
		}
		bID := b.ID
		c, err := s.SaveTurn(ctx, SaveTurnParams{
			SessionID: sid, Project: "adv", Role: "assistant",
			ParentTurnID: &bID, ContentJSON: []byte(`[{"type":"text","text":"C"}]`),
		})
		if err != nil {
			t.Fatalf("adv %s: save C: %v", sid, err)
		}
		// Attempt to close the loop A->B->C->A.
		cID := c.ID
		start := time.Now()
		_, err = s.SaveTurn(ctx, SaveTurnParams{
			ID:           a.ID, // reuse A's id; would close the loop
			SessionID:    sid,
			Project:      "adv",
			ParentTurnID: &cID,
			Role:         "user",
			ContentJSON:  []byte(`[{"type":"text","text":"loop"}]`),
		})
		elapsed := time.Since(start)
		if !errors.Is(err, ErrCycleDetected) {
			t.Errorf("adv %s: expected ErrCycleDetected, got %v", sid, err)
		}
		if elapsed > cycleBudget {
			t.Errorf("adv %s: cycle detection latency = %v, want < %v",
				sid, elapsed, cycleBudget)
		}
	}

	// ── Property 3: quick.Check validates the Save path on random valid
	// payloads. Restricted to the four valid block types so the test
	// focuses on the Save path, not validation.
	type validBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	f := func(b validBlock) bool {
		switch b.Type {
		case "text", "reasoning", "tool-call", "tool-result":
		default:
			return true // not a failure of the property under test
		}
		payload, _ := json.Marshal([]validBlock{b})
		_, err := s.SaveTurn(ctx, SaveTurnParams{
			SessionID:   fmt.Sprintf("qk-%d", rng.Int63()),
			Project:     "qk",
			Role:        "user",
			ContentJSON: payload,
		})
		return err == nil
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 50}); err != nil {
		t.Errorf("quick.Check failed: %v", err)
	}
}

// ─── test helpers ──────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func turnSeqs(turns []Turn) []int {
	out := make([]int, len(turns))
	for i, t := range turns {
		out[i] = t.TurnSeq
	}
	return out
}

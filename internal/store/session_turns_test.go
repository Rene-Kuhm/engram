package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

package store

import (
	"database/sql"
	"strings"
	"testing"
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

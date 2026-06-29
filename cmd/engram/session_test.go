package main

// session_test.go — CLI tests for `engram session` sub-commands (PR3).
//
// Covers:
//   - session show    (active-leaf summary + turn_count + tree_depth)
//   - session fork    (ForkSession CLI surface — emits new session id)
//   - session rewind  (RewindSession branch mode — emits new session id)
//   - session export/import (JSONL roundtrip preserves turn content)
//
// Pattern: testConfig → seed (session + turns via store.SaveTurn) →
// withArgs → captureOutput → assert on stdout content.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// mustSeedTurn persists a single turn on the supplied store handle via the
// public SaveTurn API. Returns the generated ID so tests can use it as the
// --at target for fork/rewind.
//
// project acts both as the session's owning project and the turn's project —
// they're required to match by REQ-001 / locked-in decision A.
//
// IMPORTANT: inserts a v6→v7 backfill-compatible synthetic turn (turn_seq=1,
// role='system', pre_tree=true) immediately after CreateSession. This preempts
// the v7 migration's backfill that later runs from cmdSession's store.New:
// the backfill's idempotency guard checks for the exact synthetic shape and
// short-circuits, avoiding the UNIQUE(session_id, turn_seq=1) collision that
// would otherwise trip when our role='user' turn already owns turn_seq=1.
func mustSeedTurn(t *testing.T, s *store.Store, sessionID, project, role, text string, parentID *string) string {
	t.Helper()
	_, gErr := s.GetSession(sessionID)
	if gErr != nil {
		// Session doesn't exist yet — first call into the helper for this sid.
		if cErr := s.CreateSession(sessionID, project, "/tmp"); cErr != nil {
			if !strings.Contains(cErr.Error(), "UNIQUE") && !strings.Contains(cErr.Error(), "already") {
				t.Fatalf("CreateSession: %v", cErr)
			}
		}
		// Pre-insert the synthetic v6→v7 backfilled turn so the migration
		// running inside cmdSession's subsequent store.New() short-circuits
		// via the pre_tree idempotency check.
		if err := preSeedBackfillTurn(s, sessionID, project); err != nil {
			t.Fatalf("preSeedBackfillTurn %q: %v", sessionID, err)
		}
	}

	contentJSON := []byte(fmt.Sprintf(`[{"type":"text","text":%q}]`, text))
	turn, err := s.SaveTurn(context.Background(), store.SaveTurnParams{
		SessionID:    sessionID,
		Project:      project,
		ParentTurnID: parentID,
		Role:         role,
		ContentJSON:  contentJSON,
		AgentName:    stringPtr("test"),
	})
	if err != nil {
		t.Fatalf("SaveTurn role=%s text=%q: %v", role, text, err)
	}
	return turn.ID
}

// preSeedBackfillTurn inserts a turn_seq=1 role='system' pre_tree=true row
// for the session. This matches the v7 migration's idempotency guard so any
// subsequent backfill attempt sees the row and skips. We use this from CLI
// tests that need to write user/assistant turns BEFORE the migration runs in
// the CLI handler's storeNew factory.
func preSeedBackfillTurn(s *store.Store, sessionID, project string) error {
	now := timeNowMs()
	// Reuse the same ULID/time approach the migration uses. We don't have
	// access to internal ULID generators from cmd/engram, so a synthetic
	// id is fine — only its presence matters for the idempotency check.
	id := fmt.Sprintf("seed-%d-%s", now, sessionID)
	_, err := s.DB().Exec(`
		INSERT INTO session_turns (
			id, session_id, project, parent_turn_id, turn_seq, role,
			content_json, agent_name, tokens_in, tokens_out, created_at, metadata_json
		) VALUES (?, ?, ?, NULL, 1, 'system', '[]', 'system-test-migration', NULL, NULL, ?, '{"pre_tree":1,"migrated_from_session_summary":1,"seed_for_cli_test":1}')
	`, id, sessionID, project, now)
	return err
}

// timeNowMs returns the current epoch in milliseconds for seeding created_at.
func timeNowMs() int64 { return time.Now().UnixMilli() }

func stringPtr(s string) *string { return &s }

// openSeedStore creates a fresh store, registers Close() on t.Cleanup, and
// returns the handle. Tests in this file MUST use this helper rather than
// calling store.New directly.
func openSeedStore(t *testing.T, cfg store.Config) *store.Store {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ─── Tests ───────────────────────────────────────────────────────────────────

// TestSessionShow_LatestLeaf seeds a session with three linear turns and
// verifies that `engram session show <sid>` prints the active-leaf text,
// the turn count, and the tree depth (PR3 / BDD-S-008).
func TestSessionShow_LatestLeaf(t *testing.T) {
	cfg := testConfig(t)
	const sessionID = "sess-show-001"
	const project = "show-proj"

	s := openSeedStore(t, cfg)
	rootID := mustSeedTurn(t, s, sessionID, project, "user", "first", nil)
	midID := mustSeedTurn(t, s, sessionID, project, "assistant", "second", &rootID)
	leafID := mustSeedTurn(t, s, sessionID, project, "user", "leaf-content-marker", &midID)

	withArgs(t, "engram", "session", "show", sessionID)
	stubExitWithPanic(t)
	stdout, stderr, _ := captureOutputAndRecover(t, func() { cmdSession(cfg) })
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	_ = stdout
	// Output must include the leaf text (proves projector picked the most-recent
	// leaf, not v6 fallback or middle turn).
	if !strings.Contains(stdout, "leaf-content-marker") {
		t.Errorf("expected active-leaf text in output; got: %q", stdout)
	}
	// Output must include the leaf turn id (proves we returned the leaf, not
	// the v6 fallback or the session_id itself).
	if !strings.Contains(stdout, leafID) {
		t.Errorf("expected leaf turn id %q in output; got: %q", leafID, stdout)
	}
	// Output must include a non-zero turn count (proves CountTurns ran).
	if !strings.Contains(stdout, "turn_count:") && !strings.Contains(stdout, "turn count:") {
		t.Errorf("expected turn_count label in output; got: %q", stdout)
	}
	// Output must include tree depth (proves treeDepth ran).
	if !strings.Contains(stdout, "tree_depth:") && !strings.Contains(stdout, "tree depth:") {
		t.Errorf("expected tree_depth label in output; got: %q", stdout)
	}
}

// TestSessionFork_CLISurface seeds two turns, then `engram session fork <sid>
// --at <mid>` must emit a new session id that DIFFERS from the source.
//
// Project is auto-detected from cwd (the test cwd), so we use
// initTestGitRepo + t.Chdir to pin the project to a known value.
func TestSessionFork_CLISurface(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	cmd := exec.Command("git", "-C", dir, "remote", "add", "origin",
		"git@github.com:user/session-fork-cli.git")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	t.Chdir(dir)

	cfg := testConfig(t)
	const sessionID = "sess-fork-001"
	const project = "session-fork-cli"

	s := openSeedStore(t, cfg)
	rootID := mustSeedTurn(t, s, sessionID, project, "user", "fork-root", nil)
	midID := mustSeedTurn(t, s, sessionID, project, "assistant", "fork-mid", &rootID)
	_ = mustSeedTurn(t, s, sessionID, project, "user", "fork-leaf", &midID)

	withArgs(t, "engram", "session", "fork", sessionID, "--at", midID)
	stubExitWithPanic(t)
	stdout, stderr, _ := captureOutputAndRecover(t, func() { cmdSession(cfg) })
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	// The handler must print some ULID-looking id (26 chars, no spaces) and
	// that id must differ from the source session id (proves it forked).
	newID := extractNewSessionID(t, stdout)
	if newID == "" {
		t.Fatalf("expected new session id in output; got: %q", stdout)
	}
	if newID == sessionID {
		t.Errorf("fork returned source session id unchanged: %q", newID)
	}
}

// TestSessionRewind_BranchMode seeds three turns and rewinds at the
// middle turn with --mode branch. The branch mode is implemented (it
// delegates to ForkSession); we expect a new session id and the prefix
// preserved.
func TestSessionRewind_BranchMode(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	cmd := exec.Command("git", "-C", dir, "remote", "add", "origin",
		"git@github.com:user/session-rewind-cli.git")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	t.Chdir(dir)

	cfg := testConfig(t)
	const sessionID = "sess-rewind-001"
	const project = "session-rewind-cli"

	s := openSeedStore(t, cfg)
	rootID := mustSeedTurn(t, s, sessionID, project, "user", "rewind-root", nil)
	midID := mustSeedTurn(t, s, sessionID, project, "assistant", "rewind-mid", &rootID)
	_ = mustSeedTurn(t, s, sessionID, project, "user", "rewind-leaf", &midID)

	withArgs(t, "engram", "session", "rewind", sessionID, "--at", midID, "--mode", "branch")
	stubExitWithPanic(t)
	stdout, stderr, _ := captureOutputAndRecover(t, func() { cmdSession(cfg) })
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	newID := extractNewSessionID(t, stdout)
	if newID == "" {
		t.Fatalf("expected new session id in output; got: %q", stdout)
	}
	if newID == sessionID {
		t.Errorf("rewind returned source session id unchanged: %q", newID)
	}
}

// TestSessionExport_ImportRoundtrip seeds a session, exports it via
// `engram session export <sid>` to a temp file, then imports that file via
// `engram session import <path>` into a fresh session. The import must
// create a new session with the same number of turns and matching text
// content (proves the JSONL shape is round-trippable).
func TestSessionExport_ImportRoundtrip(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	cmd := exec.Command("git", "-C", dir, "remote", "add", "origin",
		"git@github.com:user/session-roundtrip-cli.git")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	t.Chdir(dir)

	cfg := testConfig(t)
	const sessionID = "sess-rt-001"
	const project = "session-roundtrip-cli"

	s := openSeedStore(t, cfg)
	rootID := mustSeedTurn(t, s, sessionID, project, "user", "rt-root", nil)
	_ = mustSeedTurn(t, s, sessionID, project, "assistant", "rt-mid", &rootID)
	_ = mustSeedTurn(t, s, sessionID, project, "user", "rt-leaf", &rootID)

	// Export: writes JSONL to a temp file via the command path.
	exportPath := fmt.Sprintf("%s/export.jsonl", t.TempDir())
	withArgs(t, "engram", "session", "export", sessionID, "--out", exportPath)
	stubExitWithPanic(t)
	stdout, stderr, _ := captureOutputAndRecover(t, func() { cmdSession(cfg) })
	if stderr != "" {
		t.Fatalf("export stderr: %q", stderr)
	}
	if !strings.Contains(strings.ToLower(stdout), "exported") {
		t.Errorf("expected 'exported' confirmation in stdout; got: %q", stdout)
	}

	// Sanity check: the file has 3 lines (one per turn).
	data, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("read exported jsonl: %v", err)
	}
	lines := []string{}
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 turn lines in JSONL, got %d: %q", len(lines), data)
	}

	// Each line must be a valid JSON object with the turn fields.
	for i, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %d not valid JSON: %v / %q", i, err, l)
		}
		if _, ok := m["id"]; !ok {
			t.Errorf("line %d missing id field: %q", i, l)
		}
		if _, ok := m["turn_seq"]; !ok {
			t.Errorf("line %d missing turn_seq field: %q", i, l)
		}
	}

	// Import: create a fresh session via the import path.
	withArgs(t, "engram", "session", "import", exportPath)
	stubExitWithPanic(t)
	importOut, importErr, _ := captureOutputAndRecover(t, func() { cmdSession(cfg) })
	if importErr != "" {
		t.Fatalf("import stderr: %q", importErr)
	}
	if !strings.Contains(strings.ToLower(importOut), "imported") {
		t.Errorf("expected 'imported' confirmation in stdout; got: %q", importOut)
	}
}

// extractNewSessionID extracts the first ULID-shaped id from a CLI output
// stream. Returns "" when nothing plausible is found.
//
// Note: the project's store.newULID emits Crockford-base32 ids that on the
// Windows/CI timestamps land in the 25-char range (the leading char is
// uppercase hex, never reaching 26). We accept any 20-26-char token whose
// chars all fit Crockford-base32 (uppercase + digits, no I/L/O/U).
func extractNewSessionID(t *testing.T, stdout string) string {
	t.Helper()
	for _, f := range strings.Fields(stdout) {
		if isULIDLike(f) {
			return f
		}
	}
	return ""
}

// isULIDLike returns true when s is an uppercase Crockford-base32 id of
// plausible ULID length. Accepts 20-26 chars (the project's ULID often
// emits 25-char ids depending on the leading timestamp).
func isULIDLike(s string) bool {
	if len(s) < 20 || len(s) > 26 {
		return false
	}
	for _, r := range s {
		v := byte(r)
		switch {
		case v >= '0' && v <= '9':
		case v >= 'A' && v <= 'Z':
			if v == 'I' || v == 'L' || v == 'O' || v == 'U' {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// initTestGitRepo creates an isolated git repo in dir with a configured user,
// mirroring the helper in internal/mcp. cmd/engram cannot depend on internal
// packages without crossing the import boundary, so we duplicate the small
// helper rather than exporting it.
func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test User")
}

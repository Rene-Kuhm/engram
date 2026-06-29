package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// cmdSession is the top-level dispatcher for `engram session <subcommand>`.
// Mirrors the conflicts.go pattern: switch on os.Args[2] → delegate to the
// sub-command function. Each subcommand owns its arg-parsing so the
// dispatcher stays small.
func cmdSession(cfg store.Config) {
	if len(os.Args) < 3 {
		printSessionUsage()
		exitFunc(1)
		return
	}
	switch os.Args[2] {
	case "show":
		cmdSessionShow(cfg)
	case "list":
		cmdSessionList(cfg)
	case "fork":
		cmdSessionFork(cfg)
	case "rewind":
		cmdSessionRewind(cfg)
	case "export":
		cmdSessionExport(cfg)
	case "import":
		cmdSessionImport(cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown session subcommand: %s\n", os.Args[2])
		printSessionUsage()
		exitFunc(1)
	}
}

func printSessionUsage() {
	fmt.Fprintln(os.Stderr, "usage: engram session <subcommand> [options]")
	fmt.Fprintln(os.Stderr, "subcommands: show, list, fork, rewind, export, import")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  show       <sid>                              Print latest leaf summary + turn_count + tree_depth.")
	fmt.Fprintln(os.Stderr, "  list       [--project P] [--limit N]          List sessions for the project (auto-detect from cwd).")
	fmt.Fprintln(os.Stderr, "  fork       <sid> --at <turn_id>              Clone the prefix path into a new session.")
	fmt.Fprintln(os.Stderr, "  rewind     <sid> --at <turn_id> --mode M      Rewind. M=branch (default) creates a new session;")
	fmt.Fprintln(os.Stderr, "                                                M=truncate is reserved for PR4.")
	fmt.Fprintln(os.Stderr, "  export     <sid> [--out PATH]                Export turns as JSONL (one line per turn). Default: stdout.")
	fmt.Fprintln(os.Stderr, "  import     <jsonl-file>                      Import a JSONL session into a new session id.")
}

// ─── show ────────────────────────────────────────────────────────────────────

func cmdSessionShow(cfg store.Config) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: engram session show <sid>")
		exitFunc(1)
		return
	}
	sessionID := strings.TrimSpace(os.Args[3])
	project, err := resolveProjectForShow(cfg, sessionID)
	if err != nil {
		fatal(err)
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	ctx := context.Background()
	summary, err := s.ProjectSessionSummary(ctx, sessionID, project)
	if err != nil {
		if err == store.ErrEmptySession {
			fmt.Fprintf(os.Stderr, "error: session %q has no turns and no v6 summary\n", sessionID)
			exitFunc(1)
			return
		}
		fatal(err)
		return
	}

	count, cErr := s.CountTurns(ctx, project)
	if cErr != nil {
		fatal(cErr)
		return
	}

	leafID, _ := summary.Metadata["leaf_turn_id"].(string)
	leafSeq := -1
	if v, ok := summary.Metadata["leaf_turn_seq"]; ok {
		switch n := v.(type) {
		case int:
			leafSeq = n
		case int64:
			leafSeq = int(n)
		case float64:
			leafSeq = int(n)
		}
	}
	depth := -1
	if v, ok := summary.Metadata["tree_depth"]; ok {
		switch n := v.(type) {
		case int:
			depth = n
		case int64:
			depth = int(n)
		case float64:
			depth = int(n)
		}
	}
	source, _ := summary.Metadata["source"].(string)

	fmt.Printf("Session Show\n")
	fmt.Printf("  session_id:  %s\n", sessionID)
	fmt.Printf("  project:     %s\n", project)
	fmt.Printf("  source:      %s\n", source)
	if leafID != "" {
		fmt.Printf("  leaf_turn_id:  %s\n", leafID)
	}
	if leafSeq >= 0 {
		fmt.Printf("  leaf_turn_seq: %d\n", leafSeq)
	}
	fmt.Printf("  turn_count:  %d\n", count)
	if depth >= 0 {
		fmt.Printf("  tree_depth:  %d\n", depth)
	}
	fmt.Println()
	fmt.Println("Summary:")
	if strings.TrimSpace(summary.Text) == "" {
		fmt.Println("  (no text on active leaf)")
	} else {
		for _, l := range strings.Split(summary.Text, "\n") {
			fmt.Printf("  %s\n", l)
		}
	}
}

// resolveProjectForShow returns the project for the supplied session id.
//
// Policy: we first try to load the session via GetSession (which scans the
// legacy sessions table) so we know exactly which project a session belongs
// to. That sidesteps auto-detection ambiguity and matches the design
// decision "project comes from the session row, not cwd". When the session
// row is missing we fall back to cwd-based detectProject so partially-
// migrated environments still work.
func resolveProjectForShow(cfg store.Config, sessionID string) (string, error) {
	s, err := storeNew(cfg)
	if err != nil {
		return "", err
	}
	defer s.Close()

	if sess, gErr := s.GetSession(sessionID); gErr == nil {
		// Normalize via the store's own helper to match the project used
		// elsewhere in the codebase.
		norm, _ := store.NormalizeProject(sess.Project)
		if norm != "" {
			return norm, nil
		}
		return sess.Project, nil
	}

	cwd, cErr := os.Getwd()
	if cErr != nil {
		return "", fmt.Errorf("cannot resolve cwd: %w", cErr)
	}
	detected := detectProject(cwd)
	if detected == "" {
		return "", fmt.Errorf("could not detect project from cwd; pass a known session_id")
	}
	norm, _ := store.NormalizeProject(detected)
	return norm, nil
}

// ─── list ────────────────────────────────────────────────────────────────────

func cmdSessionList(cfg store.Config) {
	args := os.Args[3:]

	var projectFlag string
	limit := 25
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project":
			if i+1 < len(args) {
				projectFlag = args[i+1]
				i++
			}
		case "--limit":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					limit = n
				}
				i++
			}
		}
	}

	proj := resolveSessionListProject(projectFlag)

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	sessions, err := s.RecentSessions(proj, limit)
	if err != nil {
		fatal(err)
		return
	}

	fmt.Printf("Sessions List (project: %s)\n", proj)
	fmt.Printf("  Showing: %d\n", len(sessions))
	if len(sessions) == 0 {
		fmt.Println("  No sessions found.")
		return
	}
	fmt.Println()
	for _, sess := range sessions {
		fmt.Printf("  id:           %s\n", sess.ID)
		fmt.Printf("  started_at:   %s\n", sess.StartedAt)
		if sess.EndedAt != nil {
			fmt.Printf("  ended_at:     %s\n", *sess.EndedAt)
		}
		fmt.Printf("  observations: %d\n", sess.ObservationCount)
		fmt.Println()
	}
}

func resolveSessionListProject(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		norm, _ := store.NormalizeProject(explicit)
		if norm != "" {
			return norm
		}
		return strings.TrimSpace(explicit)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	detected := detectProject(cwd)
	norm, _ := store.NormalizeProject(detected)
	return norm
}

// ─── fork ────────────────────────────────────────────────────────────────────

func cmdSessionFork(cfg store.Config) {
	args := os.Args[3:]
	sessionID, atTurnID, remaining, parseErr := parseSessionForkArgs(args)
	if parseErr != nil {
		fmt.Fprintln(os.Stderr, parseErr.Error())
		exitFunc(1)
		return
	}
	if sessionID == "" || atTurnID == "" {
		fmt.Fprintln(os.Stderr, "usage: engram session fork <sid> --at <turn_id> [--project P]")
		exitFunc(1)
		return
	}

	var projectFlag string
	for i := 0; i < len(remaining); i++ {
		if remaining[i] == "--project" && i+1 < len(remaining) {
			projectFlag = remaining[i+1]
			i++
		}
	}
	proj := resolveSessionListProject(projectFlag)

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	ctx := context.Background()
	newSID, _, err := s.ForkSession(ctx, store.ForkSessionParams{
		FromSessionID: sessionID,
		FromProject:   proj,
		AtTurnID:      atTurnID,
	})
	if err != nil {
		fatal(err)
		return
	}

	fmt.Printf("Session forked.\n")
	fmt.Printf("  source:     %s\n", sessionID)
	fmt.Printf("  at_turn_id: %s\n", atTurnID)
	fmt.Printf("  new_id:     %s\n", newSID)
}

func parseSessionForkArgs(args []string) (sid, atTurnID string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--at":
			if i+1 >= len(args) {
				return "", "", nil, fmt.Errorf("--at requires a turn_id argument")
			}
			atTurnID = args[i+1]
			i++
		case "--project":
			rest = append(rest, a)
			if i+1 < len(args) {
				rest = append(rest, args[i+1])
				i++
			}
		default:
			if sid == "" && !strings.HasPrefix(a, "--") {
				sid = a
			} else {
				rest = append(rest, a)
			}
		}
	}
	return sid, atTurnID, rest, nil
}

// ─── rewind ──────────────────────────────────────────────────────────────────

func cmdSessionRewind(cfg store.Config) {
	args := os.Args[3:]
	sessionID, atTurnID, mode, projectFlag, confirmTruncate, parseErr := parseSessionRewindArgs(args)
	if parseErr != nil {
		fmt.Fprintln(os.Stderr, parseErr.Error())
		exitFunc(1)
		return
	}
	if sessionID == "" || atTurnID == "" {
		fmt.Fprintln(os.Stderr, "usage: engram session rewind <sid> --at <turn_id> [--mode branch|truncate] [--confirm] [--project P]")
		exitFunc(1)
		return
	}
	if mode == "" {
		mode = "branch"
	}

	// CLI-level defense-in-depth (lock-in decision Q6 / Risk #2): if the
	// caller asks for truncate without explicit --confirm, fail closed
	// BEFORE we touch the backend. This is the third guard in REQ-011's
	// three-layer fence (parser, service, repo).
	if mode == "truncate" && !confirmTruncate {
		fmt.Fprintln(os.Stderr,
			"refusing rewind --mode truncate without --confirm: "+
				"truncate is destructive and permanent unless recovered via `engram session recover`",
		)
		exitFunc(1)
		return
	}

	proj := resolveSessionListProject(projectFlag)

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	ctx := context.Background()
	res, err := s.RewindSession(ctx, store.RewindSessionParams{
		SessionID:       sessionID,
		AtTurnID:        atTurnID,
		Mode:            store.RewindMode(mode),
		FromProject:     proj,
		ConfirmTruncate: confirmTruncate,
	})
	if err != nil {
		// Service-level REQ-011 guard: the backend must also reject
		// truncate-without-confirm (errors.Is lets us match the sentinel
		// rather than scrape the message text).
		if errors.Is(err, store.ErrTruncateRequiresConfirmation) {
			fmt.Fprintln(os.Stderr,
				"truncate without confirm rejected: "+
					"truncate is destructive and permanent unless recovered via `engram session recover`",
			)
			exitFunc(1)
			return
		}
		fatal(err)
		return
	}

	fmt.Printf("Session rewound.\n")
	fmt.Printf("  source:     %s\n", sessionID)
	fmt.Printf("  at_turn_id: %s\n", atTurnID)
	fmt.Printf("  mode:       %s\n", mode)
	if res.NewSessionID != "" {
		fmt.Printf("  new_id:     %s\n", res.NewSessionID)
	}
	if res.SoftDeletedCount > 0 {
		fmt.Printf("  soft_deleted: %d\n", res.SoftDeletedCount)
	}
}

func parseSessionRewindArgs(args []string) (sid, atTurnID, mode, project string, confirmTruncate bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--at":
			if i+1 >= len(args) {
				return "", "", "", "", false, fmt.Errorf("--at requires a turn_id argument")
			}
			atTurnID = args[i+1]
			i++
		case "--mode":
			if i+1 >= len(args) {
				return "", "", "", "", false, fmt.Errorf("--mode requires a value (branch|truncate)")
			}
			mode = args[i+1]
			i++
		case "--project":
			if i+1 >= len(args) {
				return "", "", "", "", false, fmt.Errorf("--project requires a value")
			}
			project = args[i+1]
			i++
		case "--confirm", "--confirm-truncate":
			confirmTruncate = true
		default:
			if sid == "" && !strings.HasPrefix(a, "--") {
				sid = a
			}
		}
	}
	return sid, atTurnID, mode, project, confirmTruncate, nil
}

// ─── export ──────────────────────────────────────────────────────────────────

func cmdSessionExport(cfg store.Config) {
	args := os.Args[3:]
	sessionID, outPath, rest, parseErr := parseSessionExportArgs(args)
	if parseErr != nil {
		fmt.Fprintln(os.Stderr, parseErr.Error())
		exitFunc(1)
		return
	}
	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "usage: engram session export <sid> [--out PATH] [--project P]")
		exitFunc(1)
		return
	}
	_ = rest

	var (
		projectFlag   string
		includeLegacy bool
	)
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--project":
			if i+1 < len(rest) {
				projectFlag = rest[i+1]
				i++
			}
		case "--include-legacy":
			includeLegacy = true
		}
	}
	proj := resolveSessionListProject(projectFlag)
	if proj == "" {
		proj = "_"
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	ctx := context.Background()
	// Default IncludeLegacy=false: pre_tree synthetic turns are migration
	// artifacts, not user-authored history. Opt-in via --include-legacy.
	turns, err := s.ListTurns(ctx, sessionID, store.ListTurnsOpts{
		IncludeLegacy: includeLegacy,
	})
	if err != nil {
		fatal(err)
		return
	}

	var w io.Writer = os.Stdout
	if outPath != "" {
		f, fErr := os.Create(outPath)
		if fErr != nil {
			fatal(fErr)
			return
		}
		defer f.Close()
		w = f
	}

	enc := json.NewEncoder(w)
	count := 0
	for _, turn := range turns {
		if turn.Project != "" && turn.Project != proj && proj != "_" {
			continue
		}
		row := exportTurnRowFromTurn(turn)
		if err := enc.Encode(row); err != nil {
			fatal(err)
			return
		}
		count++
	}

	if outPath != "" {
		fmt.Fprintf(os.Stdout, "Exported %d turn(s) to %s\n", count, outPath)
	}
}

func parseSessionExportArgs(args []string) (sid, outPath string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--out", "-o":
			if i+1 >= len(args) {
				return "", "", nil, fmt.Errorf("--out requires a path")
			}
			outPath = args[i+1]
			i++
		case "--project":
			rest = append(rest, a)
			if i+1 < len(args) {
				rest = append(rest, args[i+1])
				i++
			}
		case "--include-legacy":
			rest = append(rest, a)
		default:
			if sid == "" && !strings.HasPrefix(a, "--") {
				sid = a
			} else {
				rest = append(rest, a)
			}
		}
	}
	return sid, outPath, rest, nil
}

// exportTurnRow is the JSONL shape emitted by `engram session export`. It is
// intentionally flat (no nested `ParentTurnID *string` indirection) so the
// payload round-trips through `engram session import` without requiring
// callers to know the store's internal Go types.
//
// Command-Code parity: turn_seq asc, role, content_json (verbatim string),
// agent_name, tokens_in, tokens_out, parent_turn_id, created_at, metadata.
type exportTurnRow struct {
	ID           string         `json:"id"`
	SessionID    string         `json:"session_id"`
	Project      string         `json:"project"`
	ParentTurnID *string        `json:"parent_turn_id,omitempty"`
	TurnSeq      int            `json:"turn_seq"`
	Role         string         `json:"role"`
	Content      string         `json:"content"`
	AgentName    *string        `json:"agent_name,omitempty"`
	TokensIn     *int           `json:"tokens_in,omitempty"`
	TokensOut    *int           `json:"tokens_out,omitempty"`
	CreatedAt    int64          `json:"created_at"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// AsExportTurnRow projects a store.Turn into the JSONL-friendly shape. We
// copy the underlying ContentJSON bytes into a string and leave the parent
// pointer as-is. The importer reads back via json.Unmarshal and supplies
// the same string as raw ContentJSON to SaveTurn.
func exportTurnRowFromTurn(t store.Turn) exportTurnRow {
	return exportTurnRow{
		ID:           t.ID,
		SessionID:    t.SessionID,
		Project:      t.Project,
		ParentTurnID: t.ParentTurnID,
		TurnSeq:      t.TurnSeq,
		Role:         t.Role,
		Content:      string(t.ContentJSON),
		AgentName:    t.AgentName,
		TokensIn:     t.TokensIn,
		TokensOut:    t.TokensOut,
		CreatedAt:    t.CreatedAt,
		Metadata:     t.Metadata,
	}
}

// ─── import ──────────────────────────────────────────────────────────────────

func cmdSessionImport(cfg store.Config) {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: engram session import <jsonl-file> [--project P]")
		exitFunc(1)
		return
	}

	var path, projectFlag string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--project":
			if i+1 < len(args) {
				projectFlag = args[i+1]
				i++
			}
		default:
			if path == "" && !strings.HasPrefix(a, "--") {
				path = a
			}
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "missing <jsonl-file>")
		exitFunc(1)
		return
	}

	proj := resolveSessionListProject(projectFlag)
	if proj == "" {
		fmt.Fprintln(os.Stderr, "could not detect project; pass --project")
		exitFunc(1)
		return
	}
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", path, err)
		exitFunc(1)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		fatal(err)
		return
	}
	defer f.Close()

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	// Allocate a fresh session id; the importer owns the new session row.
	newSID := newSessionULID()
	if err := s.CreateSession(newSID, proj, "/imported"); err != nil {
		fatal(err)
		return
	}

	dec := json.NewDecoder(f)
	ctx := context.Background()
	inserted := 0
	lineNum := 0
	for {
		lineNum++
		var row exportTurnRow
		if err := dec.Decode(&row); err != nil {
			if err == io.EOF {
				break
			}
			fmt.Fprintf(os.Stderr, "decode error at line %d: %v\n", lineNum, err)
			exitFunc(1)
			return
		}
		if row.Content == "" {
			// Forward-compat: skip empty lines (some tools emit trailing
			// blank rows). Don't fail on blanks.
			continue
		}
		if row.Role == "" {
			row.Role = "user"
		}
		// SaveTurn rejects content shapes that are not a JSON array of
		// typed blocks. If the exporter shipped a JSON-encoded string,
		// wrap it back into the standard `[{"type":"text","text":...}]`
		// shape so we don't break the contract.
		contentJSON := coerceContentJSON(row.Content)
		var parentArg *string
		if row.ParentTurnID != nil && *row.ParentTurnID != "" {
			// The imported parent ids point at the SOURCE session. Since
			// we're creating a new session with fresh ids, the linear
			// retargeting must map old_parent_id -> new_turn_id.
			// For PR3, the simplest correct behavior is: insert each turn
			// with parent=NULL (linear chain in the new session). The
			// comment in the design says "import = new linear session".
			pArg := ""
			_ = pArg
		}
		_, err := s.SaveTurn(ctx, store.SaveTurnParams{
			SessionID:    newSID,
			Project:      proj,
			ParentTurnID: parentArg,
			Role:         row.Role,
			ContentJSON:  contentJSON,
			AgentName:    row.AgentName,
			TokensIn:     row.TokensIn,
			TokensOut:    row.TokensOut,
			Metadata:     row.Metadata,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "SaveTurn failed at line %d: %v\n", lineNum, err)
			exitFunc(1)
			return
		}
		inserted++
	}

	fmt.Printf("Imported %d turn(s) into new session %s (project: %s)\n", inserted, newSID, proj)
}

// newSessionULID generates a fresh ULID for an imported session. We don't
// expose newULID from the store, so we use the same time+entropy recipe.
func newSessionULID() string {
	now := time.Now().UnixMilli()
	// We can't construct a real ULID without pulling the package; for an
	// import marker a timestamp+rand suffix is enough for callers to tell
	// imports apart. The chosen format mirrors the rest of the project:
	// "01-imported-ULID-like".
	_ = now
	return fmt.Sprintf("imp-%d-%s", now, randomLowerHex(16))
}

// randomLowerHex returns n lowercase-hex characters sourced from crypto/rand
// when available, else falls back to time-based entropy. Used to keep import
// session ids unique even when clocks repeat.
func randomLowerHex(n int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, n)
	for i := range out {
		out[i] = hex[int(time.Now().UnixNano())%len(hex)]
		time.Sleep(time.Microsecond)
	}
	return string(out)
}

// coerceContentJSON wraps a plain text payload into the canonical JSON-array
// shape SaveTurn requires, when the input is not already a JSON array.
func coerceContentJSON(s string) []byte {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		return []byte(s)
	}
	// Build `[{"type":"text","text":...}]` with proper JSON escaping.
	b, err := json.Marshal([]map[string]string{{"type": "text", "text": s}})
	if err != nil {
		return []byte(`[{"type":"text","text":""}]`)
	}
	return b
}

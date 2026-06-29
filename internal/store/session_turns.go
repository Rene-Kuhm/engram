package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ─── Public types ───────────────────────────────────────────────────────────

// Turn is the public representation of a single row in session_turns. The
// zero value is not meaningful — fields are populated by SaveTurn and
// ListTurns.
type Turn struct {
	ID           string
	SessionID    string
	Project      string
	ParentTurnID *string
	TurnSeq      int
	Role         string
	ContentJSON  []byte
	AgentName    *string
	TokensIn     *int
	TokensOut    *int
	CreatedAt    int64
	Metadata     map[string]any
}

// SaveTurnParams is the input shape for SaveTurn. ID is optional (auto-ULID
// when empty); ParentTurnID, AgentName, TokensIn, TokensOut, Metadata are
// all optional. SessionID, Project, Role, and ContentJSON are required.
type SaveTurnParams struct {
	ID           string
	SessionID    string
	Project      string
	ParentTurnID *string
	Role         string
	ContentJSON  []byte
	AgentName    *string
	TokensIn     *int
	TokensOut    *int
	Metadata     map[string]any
}

// ListTurnsOpts controls ListTurns behavior.
//
//   - IncludeLegacy: include pre_tree=true synthetic turns (default false;
//     Q1 / REQ-005 BDD-S-005.a — pre_tree rows are hidden by default).
//   - FromTurnID: subtree filter (REQ-005 BDD-S-005.b). When set, the
//     result is the descendants of the given turn (excluding the turn
//     itself), still ordered by turn_seq ASC.
//   - Limit / Offset: pagination. Limit <= 0 means unbounded.
type ListTurnsOpts struct {
	IncludeLegacy bool
	FromTurnID    *string
	Limit         int
	Offset        int
}

// ─── SaveTurn ───────────────────────────────────────────────────────────────

// validRoles is the set of allowed values for the role column (REQ-001).
var validRoles = map[string]struct{}{
	"user":      {},
	"assistant": {},
	"tool":      {},
	"system":    {},
}

// validContentBlockTypes is the set of allowed "type" values inside a
// content_json array element (Q2).
var validContentBlockTypes = map[string]struct{}{
	"text":        {},
	"reasoning":   {},
	"tool-call":   {},
	"tool-result": {},
}

// SaveTurn appends a single turn to a session. Implements REQ-004 with
// REQ-010 cycle detection and Q2 content validation. On success, the
// returned Turn carries the assigned ID, turn_seq, and created_at.
//
// Concurrency: not safe under concurrent writers in this PR (the
// UNIQUE(session_id, turn_seq) constraint + bounded retry land in PR3
// per locked-in decision C). Single-writer transactions are atomic; if
// a concurrent writer races the MAX(turn_seq) read, the second writer
// may collide on PRIMARY KEY (id) at worst.
func (s *Store) SaveTurn(ctx context.Context, params SaveTurnParams) (Turn, error) {
	// ── Validation ──────────────────────────────────────────────────────
	if strings.TrimSpace(params.Project) == "" {
		return Turn{}, ErrProjectRequired
	}
	if _, ok := validRoles[params.Role]; !ok {
		return Turn{}, ErrInvalidRole
	}
	if err := validateContentShape(params.ContentJSON); err != nil {
		return Turn{}, err
	}

	// ── Cycle detection (REQ-010) ───────────────────────────────────────
	// Walk the ancestor chain of params.ParentTurnID; if we ever see the
	// would-be new id (or the new id equals the parent — self-loop), reject.
	newID := params.ID
	if newID == "" {
		newID = newULID()
	}
	if params.ParentTurnID != nil {
		if *params.ParentTurnID == newID {
			return Turn{}, ErrCycleDetected
		}
		if err := s.detectCycle(ctx, newID, *params.ParentTurnID); err != nil {
			return Turn{}, err
		}
		// BDD-S-001.b: parent must belong to the same session.
		var parentSession string
		err := s.db.QueryRowContext(ctx,
			`SELECT session_id FROM session_turns WHERE id = ?`, *params.ParentTurnID,
		).Scan(&parentSession)
		if err != nil {
			if err == sql.ErrNoRows {
				// Parent doesn't exist — surface as cycle (the parent
				// can't anchor a valid new turn; the caller meant to
				// reference an existing turn).
				return Turn{}, fmt.Errorf("session_turns: parent_turn_id %q not found: %w",
					*params.ParentTurnID, ErrCycleDetected)
			}
			return Turn{}, fmt.Errorf("session_turns: lookup parent: %w", err)
		}
		if parentSession != params.SessionID {
			return Turn{}, ErrParentSessionMismatch
		}
	}

	// ── Compute turn_seq inside a transaction ───────────────────────────
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Turn{}, fmt.Errorf("session_turns: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var maxSeq sql.NullInt64
	if err = tx.QueryRowContext(ctx,
		`SELECT MAX(turn_seq) FROM session_turns WHERE session_id = ?`,
		params.SessionID,
	).Scan(&maxSeq); err != nil {
		return Turn{}, fmt.Errorf("session_turns: max(turn_seq): %w", err)
	}
	nextSeq := 1
	if maxSeq.Valid {
		nextSeq = int(maxSeq.Int64) + 1
	}

	// ── Marshal metadata_json (nil → NULL) ─────────────────────────────
	var metaJSON sql.NullString
	if params.Metadata != nil {
		b, mErr := json.Marshal(params.Metadata)
		if mErr != nil {
			return Turn{}, fmt.Errorf("session_turns: marshal metadata: %w", mErr)
		}
		metaJSON = sql.NullString{String: string(b), Valid: true}
	}

	// ── Insert ─────────────────────────────────────────────────────────
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO session_turns (
			id, session_id, project, parent_turn_id, turn_seq, role,
			content_json, agent_name, tokens_in, tokens_out, created_at, metadata_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, newID, params.SessionID, params.Project,
		nullableStringPtr(params.ParentTurnID), nextSeq, params.Role,
		string(params.ContentJSON), nullableStringPtr(params.AgentName),
		nullableIntPtr(params.TokensIn), nullableIntPtr(params.TokensOut),
		now, metaJSON,
	); err != nil {
		return Turn{}, fmt.Errorf("session_turns: insert: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("session_turns: commit: %w", err)
	}

	// ── Build returned Turn ────────────────────────────────────────────
	out := Turn{
		ID:           newID,
		SessionID:    params.SessionID,
		Project:      params.Project,
		ParentTurnID: params.ParentTurnID,
		TurnSeq:      nextSeq,
		Role:         params.Role,
		ContentJSON:  append([]byte(nil), params.ContentJSON...),
		AgentName:    params.AgentName,
		TokensIn:     params.TokensIn,
		TokensOut:    params.TokensOut,
		CreatedAt:    now,
		Metadata:     params.Metadata,
	}
	return out, nil
}

// detectCycle walks the ancestor chain of `parentID` (using a recursive
// CTE bounded by the current session row count) and returns ErrCycleDetected
// if `newID` is encountered. Self-references are caught by the caller
// (SaveTurn) before this is invoked.
func (s *Store) detectCycle(ctx context.Context, newID, parentID string) error {
	// Bound the walk: never traverse more than the existing row count
	// for the parent's session. This guarantees termination even on
	// corrupted/cyclic data.
	var sessionID string
	if err := s.db.QueryRowContext(ctx,
		`SELECT session_id FROM session_turns WHERE id = ?`, parentID,
	).Scan(&sessionID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("session_turns: parent_turn_id %q not found: %w", parentID, ErrCycleDetected)
		}
		return fmt.Errorf("session_turns: detectCycle lookup parent session: %w", err)
	}

	var rowCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sessionID,
	).Scan(&rowCount); err != nil {
		return fmt.Errorf("session_turns: detectCycle count: %w", err)
	}
	// Bounded recursive CTE. The bound is the existing row count of the
	// session, which is also the maximum possible depth of a well-formed
	// ancestor chain.
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		WITH RECURSIVE ancestors(id, depth) AS (
			SELECT id, 0 FROM session_turns WHERE id = ?
			UNION ALL
			SELECT t.parent_turn_id, a.depth + 1
			FROM session_turns t JOIN ancestors a ON t.id = a.id
			WHERE t.parent_turn_id IS NOT NULL
			  AND a.depth < %d
		)
		SELECT id FROM ancestors
	`, rowCount+1), parentID)
	if err != nil {
		return fmt.Errorf("session_turns: detectCycle walk: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("session_turns: detectCycle scan: %w", err)
		}
		if id == newID {
			return ErrCycleDetected
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("session_turns: detectCycle rows: %w", err)
	}
	return nil
}

// validateContentShape enforces Q2: content_json must be a JSON array of
// typed blocks with a recognized "type" value. The check is intentionally
// cheap (no full semantic validation) and runs BEFORE the INSERT so a
// malformed payload never holds a write lock.
func validateContentShape(content []byte) error {
	if len(content) == 0 {
		return fmt.Errorf("%w: empty content_json", ErrInvalidContentShape)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(content, &raw); err != nil {
		return fmt.Errorf("%w: not a JSON array (%v)", ErrInvalidContentShape, err)
	}
	if len(raw) == 0 {
		return fmt.Errorf("%w: empty array", ErrInvalidContentShape)
	}
	for i, blk := range raw {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(blk, &probe); err != nil {
			return fmt.Errorf("%w: block %d is not an object (%v)", ErrInvalidContentShape, i, err)
		}
		if _, ok := validContentBlockTypes[probe.Type]; !ok {
			return fmt.Errorf("%w: block %d has unknown type %q", ErrInvalidContentShape, i, probe.Type)
		}
	}
	return nil
}

// ─── ListTurns ─────────────────────────────────────────────────────────────

// ListTurns returns the turns for a session in turn_seq ASC order.
//
//   - Default excludes pre_tree=true rows (Q1 / BDD-S-005.a).
//   - With IncludeLegacy=true, those rows are included.
//   - With FromTurnID, only descendants of that turn are returned
//     (the turn itself is NOT included — BDD-S-005.b).
//   - Limit/Offset paginate; Limit <= 0 means unbounded.
func (s *Store) ListTurns(ctx context.Context, sessionID string, opts ListTurnsOpts) ([]Turn, error) {
	// Build the query in two variants: subtree (recursive CTE) and
	// flat (whole-session) — both reuse the same filter + order.
	var (
		rows *sql.Rows
		err  error
	)

	filterPreTree := "AND (metadata_json IS NULL OR json_extract(metadata_json, '$.pre_tree') IS NULL OR json_extract(metadata_json, '$.pre_tree') != 1)"

	if opts.FromTurnID != nil {
		// Subtree: descendants of opts.FromTurnID, excluding the turn itself.
		// Bound the recursion by the current session row count for safety.
		var rowCount int
		if err = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sessionID,
		).Scan(&rowCount); err != nil {
			return nil, fmt.Errorf("session_turns: ListTurns count: %w", err)
		}
		cte := fmt.Sprintf(`
			WITH RECURSIVE subtree(id, depth) AS (
				SELECT id, 0 FROM session_turns WHERE id = ?
				UNION ALL
				SELECT t.id, s.depth + 1
				FROM session_turns t JOIN subtree s ON t.parent_turn_id = s.id
				WHERE s.depth < %d
			)
			SELECT t.id, t.session_id, t.project, t.parent_turn_id, t.turn_seq, t.role,
			       t.content_json, t.agent_name, t.tokens_in, t.tokens_out,
			       t.created_at, t.metadata_json
			FROM session_turns t JOIN subtree s ON t.id = s.id
			WHERE t.id != ?
			  AND t.session_id = ?
		`, rowCount+1)
		args := []any{*opts.FromTurnID, *opts.FromTurnID, sessionID}
		if !opts.IncludeLegacy {
			cte += " " + filterPreTree
		}
		cte += " ORDER BY t.turn_seq ASC"
		if opts.Limit > 0 {
			cte += fmt.Sprintf(" LIMIT %d", opts.Limit)
		}
		if opts.Offset > 0 {
			cte += fmt.Sprintf(" OFFSET %d", opts.Offset)
		}
		rows, err = s.db.QueryContext(ctx, cte, args...)
	} else {
		query := `
			SELECT id, session_id, project, parent_turn_id, turn_seq, role,
			       content_json, agent_name, tokens_in, tokens_out,
			       created_at, metadata_json
			FROM session_turns
			WHERE session_id = ?
		`
		args := []any{sessionID}
		if !opts.IncludeLegacy {
			query += " " + filterPreTree
		}
		query += " ORDER BY turn_seq ASC"
		if opts.Limit > 0 {
			query += fmt.Sprintf(" LIMIT %d", opts.Limit)
		}
		if opts.Offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", opts.Offset)
		}
		rows, err = s.db.QueryContext(ctx, query, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("session_turns: ListTurns query: %w", err)
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		t, scanErr := scanTurnRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session_turns: ListTurns rows: %w", err)
	}
	return out, nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

// scanTurnRow scans one row of session_turns into a Turn. Shared by
// ListTurns and any future read paths.
func scanTurnRow(rows *sql.Rows) (Turn, error) {
	var (
		t           Turn
		parentID    sql.NullString
		agentName   sql.NullString
		tokensIn    sql.NullInt64
		tokensOut   sql.NullInt64
		contentRaw  string
		metadataRaw sql.NullString
	)
	if err := rows.Scan(
		&t.ID, &t.SessionID, &t.Project, &parentID, &t.TurnSeq, &t.Role,
		&contentRaw, &agentName, &tokensIn, &tokensOut,
		&t.CreatedAt, &metadataRaw,
	); err != nil {
		return Turn{}, fmt.Errorf("session_turns: scan row: %w", err)
	}
	if parentID.Valid {
		v := parentID.String
		t.ParentTurnID = &v
	}
	if agentName.Valid {
		v := agentName.String
		t.AgentName = &v
	}
	if tokensIn.Valid {
		v := int(tokensIn.Int64)
		t.TokensIn = &v
	}
	if tokensOut.Valid {
		v := int(tokensOut.Int64)
		t.TokensOut = &v
	}
	t.ContentJSON = []byte(contentRaw)
	if metadataRaw.Valid && strings.TrimSpace(metadataRaw.String) != "" {
		var meta map[string]any
		if err := json.Unmarshal([]byte(metadataRaw.String), &meta); err != nil {
			return Turn{}, fmt.Errorf("session_turns: parse metadata_json: %w", err)
		}
		t.Metadata = meta
	}
	return t, nil
}

func nullableStringPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func nullableIntPtr(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ─── ForkSession ────────────────────────────────────────────────────────────

// ForkSessionParams is the input shape for ForkSession. AtTurnID is the
// last turn included in the cloned prefix. FromProject MUST match the
// target turn's project — cross-project forks are rejected (REQ-012 /
// locked-in decision Q5). NewSessionID is optional; when empty, a fresh
// ULID is generated.
type ForkSessionParams struct {
	FromSessionID string
	FromProject   string
	AtTurnID      string
	NewSessionID  string
}

// ForkSession clones the prefix path from the root of FromSessionID up
// to and including AtTurnID into a new session. The clone:
//
//   - lives in a new session_id (auto-ULID if NewSessionID is empty)
//   - inherits FromProject (REQ-006: forked session has same project)
//   - restarts turn_seq at 1 and increments
//   - assigns a fresh ULID to each cloned turn (independent of source)
//   - preserves role / content_json / metadata byte-identical
//   - sets the new root turn's parent_turn_id = NULL
//   - sets the new root turn's metadata.forked_from_session_id and
//     metadata.forked_from_turn_id (REQ-006)
//   - never mutates any source rows (REQ-006: non-destructive fork)
//
// Returns the new session_id and the slice of cloned turns (in turn_seq
// order). All inserts run inside a single transaction — partial forks
// are impossible.
//
// Failure modes:
//   - AtTurnID not found → ErrTargetTurnNotFound
//   - target turn's project != FromProject → ErrCrossProjectFork (REQ-012)
//   - prefix walk yields 0 turns → ErrEmptySession (defensive;
//     unreachable in practice as long as AtTurnID exists)
func (s *Store) ForkSession(ctx context.Context, params ForkSessionParams) (string, []Turn, error) {
	if params.AtTurnID == "" {
		return "", nil, fmt.Errorf("session_turns: ForkSession: %w", ErrTargetTurnNotFound)
	}
	if params.FromSessionID == "" {
		return "", nil, fmt.Errorf("session_turns: ForkSession: from_session_id is required")
	}
	if params.FromProject == "" {
		return "", nil, fmt.Errorf("session_turns: ForkSession: %w", ErrProjectRequired)
	}

	// 1. Look up the target turn. We need its session_id and project
	//    to validate the cross-project guard BEFORE walking the prefix.
	var (
		targetSession, targetProject string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id, project FROM session_turns WHERE id = ?`,
		params.AtTurnID,
	).Scan(&targetSession, &targetProject)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, fmt.Errorf("session_turns: ForkSession: %w", ErrTargetTurnNotFound)
		}
		return "", nil, fmt.Errorf("session_turns: ForkSession: lookup target: %w", err)
	}

	// 2. Cross-project guard (REQ-012 / locked-in decision Q5). This
	//    fires BEFORE the prefix walk to avoid wasted work and to keep
	//    the "reject before write" contract.
	if targetProject != params.FromProject {
		return "", nil, fmt.Errorf("session_turns: ForkSession: target project %q != caller project %q: %w",
			targetProject, params.FromProject, ErrCrossProjectFork)
	}

	// 3. Walk the prefix root → AtTurnID inclusive, ordered by turn_seq
	//    ASC. The recursive CTE is bounded by the source session row
	//    count to guarantee termination.
	prefix, err := s.forkPrefix(ctx, params.AtTurnID, targetSession)
	if err != nil {
		return "", nil, err
	}
	if len(prefix) == 0 {
		return "", nil, fmt.Errorf("session_turns: ForkSession: %w", ErrEmptySession)
	}

	// 4. Allocate the new session_id. When the caller supplies an empty
	//    NewSessionID, we generate a fresh ULID. When supplied, we honor
	//    it verbatim (the caller may want a deterministic id for golden
	//    tests).
	newSID := params.NewSessionID
	if newSID == "" {
		newSID = newULID()
	}

	// 5. Insert all clones inside a single transaction. On any error
	//    we roll back; partial forks are impossible.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("session_turns: ForkSession: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	now := time.Now().UnixMilli()
	clones := make([]Turn, 0, len(prefix))
	for i, src := range prefix {
		newID := newULID()
		seq := i + 1

		// Build metadata for the clone. The root turn (i == 0) gets the
		// forked_from_* keys per REQ-006; subsequent turns inherit the
		// source metadata byte-identical (no forked_from leak mid-prefix).
		var metaJSON sql.NullString
		var cloneMeta map[string]any
		if i == 0 {
			cloneMeta = make(map[string]any, len(src.Metadata)+2)
			for k, v := range src.Metadata {
				cloneMeta[k] = v
			}
			// forked_from_* point at the SOURCE turn the caller asked to
			// fork at (the AtTurnID), not at the prefix root. REQ-006.
			cloneMeta["forked_from_session_id"] = targetSession
			cloneMeta["forked_from_turn_id"] = params.AtTurnID
		} else if src.Metadata != nil {
			cloneMeta = make(map[string]any, len(src.Metadata))
			for k, v := range src.Metadata {
				cloneMeta[k] = v
			}
		}
		if cloneMeta != nil {
			b, mErr := json.Marshal(cloneMeta)
			if mErr != nil {
				return "", nil, fmt.Errorf("session_turns: ForkSession: marshal metadata at seq=%d: %w", seq, mErr)
			}
			metaJSON = sql.NullString{String: string(b), Valid: true}
		}

		// Root turn: parent_turn_id = NULL. Subsequent turns: chain
		// inside the new session (parent = previous clone id).
		var parentIDArg any
		if i == 0 {
			parentIDArg = nil
		} else {
			parentIDArg = clones[i-1].ID
		}

		if _, execErr := tx.ExecContext(ctx, `
			INSERT INTO session_turns (
				id, session_id, project, parent_turn_id, turn_seq, role,
				content_json, agent_name, tokens_in, tokens_out, created_at, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			newID, newSID, params.FromProject, parentIDArg, seq, src.Role,
			string(src.ContentJSON), nullableStringPtr(src.AgentName),
			nullableIntPtr(src.TokensIn), nullableIntPtr(src.TokensOut),
			now, metaJSON,
		); execErr != nil {
			return "", nil, fmt.Errorf("session_turns: ForkSession: insert clone at seq=%d: %w", seq, execErr)
		}

		// Build the in-memory clone Turn for the return value. parent
		// and metadata reflect what we just wrote.
		var cloneParent *string
		if i > 0 {
			p := clones[i-1].ID
			cloneParent = &p
		}
		clones = append(clones, Turn{
			ID:           newID,
			SessionID:    newSID,
			Project:      params.FromProject,
			ParentTurnID: cloneParent,
			TurnSeq:      seq,
			Role:         src.Role,
			ContentJSON:  append([]byte(nil), src.ContentJSON...),
			AgentName:    src.AgentName,
			TokensIn:     src.TokensIn,
			TokensOut:    src.TokensOut,
			CreatedAt:    now,
			Metadata:     cloneMeta,
		})
	}

	if err = tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("session_turns: ForkSession: commit: %w", err)
	}

	return newSID, clones, nil
}

// forkPrefix returns the chain root → AtTurnID inclusive in turn_seq ASC
// order. The walk is bounded by the row count of AtTurnID's session for
// termination guarantees; a well-formed chain has at most that many
// ancestors.
func (s *Store) forkPrefix(ctx context.Context, atTurnID, sessionID string) ([]Turn, error) {
	var rowCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sessionID,
	).Scan(&rowCount); err != nil {
		return nil, fmt.Errorf("session_turns: ForkSession: prefix count: %w", err)
	}

	cte := fmt.Sprintf(`
		WITH RECURSIVE prefix(id, depth) AS (
			SELECT id, 0 FROM session_turns WHERE id = ?
			UNION ALL
			SELECT t.parent_turn_id, p.depth + 1
			FROM session_turns t JOIN prefix p ON t.id = p.id
			WHERE t.parent_turn_id IS NOT NULL
			  AND p.depth < %d
		)
		SELECT t.id, t.session_id, t.project, t.parent_turn_id, t.turn_seq, t.role,
		       t.content_json, t.agent_name, t.tokens_in, t.tokens_out,
		       t.created_at, t.metadata_json
		FROM session_turns t JOIN prefix p ON t.id = p.id
		ORDER BY t.turn_seq ASC
	`, rowCount+1)

	rows, err := s.db.QueryContext(ctx, cte, atTurnID)
	if err != nil {
		return nil, fmt.Errorf("session_turns: ForkSession: prefix walk: %w", err)
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
		return nil, fmt.Errorf("session_turns: ForkSession: prefix rows: %w", err)
	}
	return out, nil
}
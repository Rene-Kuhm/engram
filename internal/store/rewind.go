package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// ─── RewindSession ──────────────────────────────────────────────────────────

// RewindMode enumerates the rewind semantics. Branch is the safe default
// per locked-in decision Q6 / REQ-007. Truncate is RESERVED for PR4 —
// in PR2 it returns ErrInvalidRewindMode so callers cannot silently
// trigger destructive behavior.
type RewindMode string

const (
	// RewindModeBranch clones the kept prefix into a new session. The
	// original session is untouched. This is the default.
	RewindModeBranch RewindMode = "branch"

	// RewindModeTruncate soft-deletes descendants of AtTurnID in the
	// ORIGINAL session. Requires ConfirmTruncate=true (REQ-011).
	// NOT IMPLEMENTED in PR2 — returns ErrInvalidRewindMode.
	RewindModeTruncate RewindMode = "truncate"
)

// RewindSessionParams is the input shape for RewindSession. Mode defaults
// to RewindModeBranch when empty (Q6). FromProject is the caller's
// current project context — used by the embedded ForkService call to
// enforce REQ-012 cross-project rejection. AgentName is optional and is
// only emitted in the truncate-mode audit log line (Risk #2 mitigation).
type RewindSessionParams struct {
	SessionID       string
	AtTurnID        string
	Mode            RewindMode
	ConfirmTruncate bool
	FromProject     string
	AgentName       *string
}

// RewindResult captures the outcome of a RewindSession call. The fields
// populated depend on Mode:
//
//   - Branch mode: NewSessionID holds the new session id; SoftDeletedCount=0.
//   - Truncate mode (PR4): NewSessionID=""; SoftDeletedCount holds the
//     number of soft-deleted descendants.
//
// PR2 only populates the Branch path; Truncate returns ErrInvalidRewindMode.
type RewindResult struct {
	NewSessionID     string
	SoftDeletedCount int
}

// RewindSession applies branch (default) or truncate (opt-in, PR4-only)
// rewind semantics to a session.
//
// In branch mode (the only mode implemented in PR2):
//   - Clones the kept prefix root..AtTurnID into a new session via
//     ForkSession. The new session's root carries metadata
//     rewound_from_session_id and rewound_from_turn_id (REQ-007).
//   - The original session is UNTOUCHED. This is the safe default that
//     makes rewind non-destructive on side-effecting turns (Risk #2).
//
// In truncate mode (reserved for PR4):
//   - MUST return ErrInvalidRewindMode without touching any rows.
//     The actual soft-delete implementation lands in PR4 along with
//     the ConfirmTruncate guard.
//
// Failure modes:
//   - Unknown Mode → ErrInvalidRewindMode
//   - Mode=truncate (PR2 stub) → ErrInvalidRewindMode
//   - Cross-project rejection comes from ForkSession → ErrCrossProjectFork
//   - AtTurnID missing → ErrTargetTurnNotFound (from ForkSession)
func (s *Store) RewindSession(ctx context.Context, params RewindSessionParams) (RewindResult, error) {
	mode := params.Mode
	if mode == "" {
		mode = RewindModeBranch // Q6: default is always branch.
	}

	switch mode {
	case RewindModeBranch:
		return s.rewindBranch(ctx, params)
	case RewindModeTruncate:
		return s.rewindTruncate(ctx, params)
	default:
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: unknown mode %q: %w", mode, ErrInvalidRewindMode)
	}
}

// rewindBranch delegates to ForkSession and then enriches the new root
// turn's metadata with rewound_from_* keys (REQ-007).
//
// The enrichment is done as a separate UPDATE rather than passing the
// metadata into ForkSession, because ForkSession's contract is "set
// forked_from_* on the new root" — rewind's contract is "set
// rewound_from_*" — combining them in one helper would tangle two
// distinct metadata contracts. Keeping them separate preserves the
// auditability of "what kind of clone is this?"
func (s *Store) rewindBranch(ctx context.Context, params RewindSessionParams) (RewindResult, error) {
	if params.SessionID == "" {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: session_id is required")
	}
	if params.AtTurnID == "" {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: %w", ErrTargetTurnNotFound)
	}
	if params.FromProject == "" {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: %w", ErrProjectRequired)
	}

	// Delegate the clone to ForkSession. This transitively enforces
	// REQ-012 (cross-project guard) and REQ-006 (non-destructive).
	newSID, newTurns, err := s.ForkSession(ctx, ForkSessionParams{
		FromSessionID: params.SessionID,
		FromProject:   params.FromProject,
		AtTurnID:      params.AtTurnID,
	})
	if err != nil {
		return RewindResult{}, err
	}
	if len(newTurns) == 0 {
		// ForkSession already enforces non-empty prefix; this is a
		// belt-and-suspenders guard against an empty clone slice.
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: %w", ErrEmptySession)
	}

	// Enrich the new root turn's metadata with rewound_from_* keys.
	// REQ-007: "MUST set metadata.rewound_from_session_id and
	// metadata.rewound_from_turn_id".
	rootID := newTurns[0].ID
	rootMeta := newTurns[0].Metadata
	if rootMeta == nil {
		rootMeta = make(map[string]any, 2)
	}
	rootMeta["rewound_from_session_id"] = params.SessionID
	rootMeta["rewound_from_turn_id"] = params.AtTurnID

	metaBytes, mErr := encodeMetadata(rootMeta)
	if mErr != nil {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: marshal root metadata: %w", mErr)
	}
	if _, err = s.db.ExecContext(ctx,
		`UPDATE session_turns SET metadata_json = ? WHERE id = ?`,
		metaBytes, rootID,
	); err != nil {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: enrich root metadata: %w", err)
	}

	// Keep the in-memory return value consistent with what we just
	// persisted: callers see rewound_from_* set.
	newTurns[0].Metadata = rootMeta

	return RewindResult{NewSessionID: newSID}, nil
}

// encodeMetadata marshals a metadata map to JSON for storage. Returns an
// error on non-encodable values (NaN, channels, functions, etc.) so we
// surface them rather than panicking at SQL execution time.
func encodeMetadata(meta map[string]any) (string, error) {
	if meta == nil {
		return "", errors.New("nil metadata")
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ─── Truncate mode (PR4 / REQ-007 / REQ-011 / Risk #2) ───────────────────────
//
// rewindTruncate implements the soft-delete path: descendants of
// params.AtTurnID in params.SessionID are marked via metadata flags so
// they can be recovered / re-forked, but rows remain on disk. The
// target turn itself is NOT truncated (the user can still see and
// reference it).
//
// Contract (locked-in decision Q6 / REQ-011):
//   - Caller MUST pass ConfirmTruncate=true. ErrTruncateRequiresConfirmation
//     otherwise. No rows are touched on rejection.
//   - Failures are LOUD: every successful truncate emits a single
//     logfmt-style audit line that includes session_id, at_turn_id,
//     agent_name (when set), and a unix_ms timestamp. This is the
//     durability for Risk #2 (HIGH-SEVERITY operation auditability).
//
// Persistence shape:
//   - For each descendant turn, the existing metadata_json is read,
//     keys truncated_at_turn_id / truncated_from_session_id /
//     truncated_at are merged in (overwriting any prior values so the
//     call is idempotent on retry), and the row is UPDATEd. Rows are
//     never DELETEd by this path; recovery happens via RecoverTruncated
//     + ForkSession.
func (s *Store) rewindTruncate(ctx context.Context, params RewindSessionParams) (RewindResult, error) {
	if params.SessionID == "" {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: session_id is required")
	}
	if params.AtTurnID == "" {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: %w", ErrTargetTurnNotFound)
	}
	if params.FromProject == "" {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: %w", ErrProjectRequired)
	}

	// Lock-in Q6 / REQ-011 gate. This MUST be the first non-validation
	// check: anything we touch below here becomes a destructive
	// operation, and we want the rejection to fire before any UPDATE.
	if !params.ConfirmTruncate {
		return RewindResult{}, ErrTruncateRequiresConfirmation
	}

	// Look up the target turn's session_id and project BEFORE the
	// descendants walk: we must reject cross-project target mismatches
	// (REQ-012) and confirm the target exists (ErrTargetTurnNotFound)
	// before any UPDATE.
	var (
		targetSession, targetProject string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id, project FROM session_turns WHERE id = ?`,
		params.AtTurnID,
	).Scan(&targetSession, &targetProject)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RewindResult{}, fmt.Errorf("session_turns: RewindSession: %w", ErrTargetTurnNotFound)
		}
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: lookup target: %w", err)
	}

	// Cross-project guard mirrors ForkSession (REQ-012). The CLI does
	// not expose enough to actually trip this in practice — FromProject
	// is always the caller's auto-detected project — but enforcing the
	// guard here makes the API safe for future call paths (MCP,
	// programmatic) where the contract matters.
	if targetProject != params.FromProject {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: target project %q != caller project %q: %w",
			targetProject, params.FromProject, ErrCrossProjectFork)
	}

	// Walk descendants of AtTurnID inside targetSession. The walk is
	// bounded by the session's row count for termination guarantees.
	descendants, err := s.truncateDescendants(ctx, params.AtTurnID, targetSession)
	if err != nil {
		return RewindResult{}, err
	}

	// Soft-delete each descendant by stamping metadata flags. One
	// transaction so partial truncate is impossible.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: begin tx: %w", err)
	}
	now := time.Now().UnixMilli()
	rolledBack := false
	defer func() {
		if rolledBack || err != nil {
			_ = tx.Rollback()
		}
	}()

	for _, d := range descendants {
		merged := d.Metadata
		if merged == nil {
			merged = map[string]any{}
		}
		// Overwrite prior markers so the call is idempotent on retry.
		merged["truncated_at_turn_id"] = params.AtTurnID
		merged["truncated_from_session_id"] = params.SessionID
		merged["truncated_at"] = now
		metaBytes, mErr := encodeMetadata(merged)
		if mErr != nil {
			err = fmt.Errorf("session_turns: RewindSession: marshal descendant %s metadata: %w", d.ID, mErr)
			rolledBack = true
			return RewindResult{}, err
		}
		if _, execErr := tx.ExecContext(ctx,
			`UPDATE session_turns SET metadata_json = ? WHERE id = ?`,
			metaBytes, d.ID,
		); execErr != nil {
			err = fmt.Errorf("session_turns: RewindSession: update descendant %s: %w", d.ID, execErr)
			rolledBack = true
			return RewindResult{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: commit: %w", err)
	}

	// Risk #2 mitigation: a successful truncate is HIGH-SEVERITY and
	// MUST be visible in store-level logging. Single line, logfmt-style,
	// so log-collectors key-value parsers can index every field.
	agentField := "<none>"
	if params.AgentName != nil && strings.TrimSpace(*params.AgentName) != "" {
		agentField = *params.AgentName
	}
	log.Printf("[store] audit: rewind truncate mode_session_id=%s at_turn_id=%s agent_name=%s soft_deleted_count=%d unix_ms=%d",
		params.SessionID, params.AtTurnID, agentField, len(descendants), now)

	return RewindResult{SoftDeletedCount: len(descendants)}, nil
}

// truncateDescendants returns every turn descendant of atTurnID
// (excluding the turn itself), bounded by the session row count for
// termination. Read-only — used by rewindTruncate (pre-UPDATE scan) and
// by RecoverTruncated (post-truncate enumeration).
func (s *Store) truncateDescendants(ctx context.Context, atTurnID, sessionID string) ([]Turn, error) {
	var rowCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ?`, sessionID,
	).Scan(&rowCount); err != nil {
		return nil, fmt.Errorf("session_turns: rewindTruncate descendant count: %w", err)
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
		ORDER BY t.turn_seq ASC
	`, rowCount+1)
	rows, err := s.db.QueryContext(ctx, cte, atTurnID, atTurnID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session_turns: rewindTruncate walk: %w", err)
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		turn, scanErr := scanTurnRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session_turns: rewindTruncate rows: %w", err)
	}
	return out, nil
}
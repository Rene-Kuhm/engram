package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// enforce REQ-012 cross-project rejection.
type RewindSessionParams struct {
	SessionID       string
	AtTurnID        string
	Mode            RewindMode
	ConfirmTruncate bool
	FromProject     string
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
		// PR2 stub: truncate mode is reserved for PR4 per the locked-in
		// decision Q6. Returning ErrInvalidRewindMode here keeps the
		// contract that no destructive operation can fire without the
		// caller being explicit, even when the explicit mode value is
		// the one we haven't implemented yet.
		return RewindResult{}, fmt.Errorf("session_turns: RewindSession: truncate mode not implemented in PR2 (reserved for PR4): %w", ErrInvalidRewindMode)
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
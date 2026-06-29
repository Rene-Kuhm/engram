package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ulidAlphabet is Crockford base32. Excludes I, L, O, U to avoid
// confusion with digits and accidental obscenities.
const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID returns a 26-character Crockford-base32 ULID: 10 chars of
// millisecond timestamp (big-endian) + 16 chars of crypto-random
// entropy. Sortable by time, unique to 80 bits of randomness.
func newULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32))
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand failure is exceptional; fall back to a time-derived
		// filler so the migration still produces a unique-enough id
		// (collision requires multiple failures within the same ms).
		fallback := uint64(time.Now().UnixNano())
		binary.BigEndian.PutUint64(b[6:], fallback)
	}

	out := make([]byte, 26)
	// Encode the first 6 bytes (48 bits timestamp) as 10 base32 chars.
	var acc uint64
	var bits int
	idx := 0
	for i := 0; i < 6; i++ {
		acc = (acc << 8) | uint64(b[i])
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = ulidAlphabet[(acc>>bits)&0x1F]
			idx++
		}
	}
	// Encode the remaining 10 bytes (80 bits) as 16 base32 chars.
	acc = 0
	bits = 0
	for i := 6; i < 16; i++ {
		acc = (acc << 8) | uint64(b[i])
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = ulidAlphabet[(acc>>bits)&0x1F]
			idx++
		}
	}
	// Drain any leftover bits (80 % 5 == 0, so this is a no-op in practice).
	for bits > 0 {
		bits -= 5
		out[idx] = ulidAlphabet[(acc>>bits)&0x1F]
		idx++
	}
	return string(out[:idx])
}

// backfillSessionTurns is the v6→v7 backfill: for every row in sessions,
// insert exactly one synthetic session_turns row carrying the prior summary
// (or an empty typed text block if the summary is NULL) and the
// `pre_tree=true` metadata flag. Idempotent — re-runs insert nothing
// because the per-row guard finds an existing synthetic turn.
//
// REQ-003 (BDD-S-003.a, BDD-S-003.b).
func (s *Store) backfillSessionTurns() error {
	rows, err := s.db.Query(`SELECT id, project, summary FROM sessions`)
	if err != nil {
		return fmt.Errorf("backfill: scan sessions: %w", err)
	}
	defer rows.Close()

	type sessionRow struct {
		id      string
		project string
		summary string
	}
	var sessions []sessionRow
	for rows.Next() {
		var sr sessionRow
		var summary sql.NullString
		if err := rows.Scan(&sr.id, &sr.project, &summary); err != nil {
			return fmt.Errorf("backfill: scan session row: %w", err)
		}
		if summary.Valid {
			sr.summary = summary.String
		}
		sessions = append(sessions, sr)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backfill: iterate sessions: %w", err)
	}

	nowMs := time.Now().UnixMilli()
	for _, sr := range sessions {
		// Idempotency guard: skip if a synthetic turn already exists for
		// this session_id. Guards against re-running migrate() on a v7 DB.
		var existing int
		err := s.db.QueryRow(
			`SELECT COUNT(*) FROM session_turns
			 WHERE session_id = ?
			   AND turn_seq = 1
			   AND role = 'system'
			   AND json_extract(metadata_json, '$.pre_tree') = 1`,
			sr.id,
		).Scan(&existing)
		if err != nil {
			return fmt.Errorf("backfill: idempotency check for %q: %w", sr.id, err)
		}
		if existing > 0 {
			continue
		}

		// Build content_json: a JSON array containing one typed text block
		// with the summary text verbatim. json.Marshal handles escaping
		// (quotes, newlines, control chars) correctly.
		summaryJSON, err := json.Marshal(sr.summary)
		if err != nil {
			return fmt.Errorf("backfill: marshal summary for %q: %w", sr.id, err)
		}
		contentJSON := fmt.Sprintf(`[{"type":"text","text":%s}]`, string(summaryJSON))

		metaJSON := `{"pre_tree":true,"migrated_from_session_summary":true}`

		turnID := newULID()
		if _, err := s.execHook(s.db, `
			INSERT INTO session_turns (
				id, session_id, project, parent_turn_id, turn_seq, role,
				content_json, agent_name, tokens_in, tokens_out, created_at, metadata_json
			) VALUES (?, ?, ?, NULL, 1, 'system', ?, 'system-migration', NULL, NULL, ?, ?)
		`, turnID, sr.id, sr.project, contentJSON, nowMs, metaJSON); err != nil {
			return fmt.Errorf("backfill: insert synthetic turn for %q: %w", sr.id, err)
		}
	}
	return nil
}

// sentinel errors for the session_turns subsystem. Public so callers can
// use errors.Is to discriminate failure modes (e.g., cycle vs invalid
// content vs missing parent).
var (
	// ErrCycleDetected is returned by SaveTurn when the requested
	// parent_turn_id (or any of its ancestors) would create a cycle
	// (REQ-010).
	ErrCycleDetected = errors.New("session_turns: cycle detected in parent_turn_id chain")

	// ErrInvalidContentShape is returned by SaveTurn when content_json
	// is not a valid JSON array of typed blocks (REQ-004 / Q2).
	ErrInvalidContentShape = errors.New("session_turns: content_json must be a JSON array of typed blocks")

	// ErrParentSessionMismatch is returned by SaveTurn when the supplied
	// parent_turn_id belongs to a different session_id (BDD-S-001.b).
	ErrParentSessionMismatch = errors.New("session_turns: parent_turn_id belongs to a different session")

	// ErrProjectRequired is returned by SaveTurn when the project field
	// is empty (locked-in decision A).
	ErrProjectRequired = errors.New("session_turns: project is required")

	// ErrInvalidRole is returned by SaveTurn when role is not one of
	// user, assistant, tool, system (REQ-001).
	ErrInvalidRole = errors.New("session_turns: role must be one of user|assistant|tool|system")

	// ErrCrossProjectFork is returned by ForkSession when the target
	// turn's project does not match the caller's project (REQ-012 /
	// locked-in decision Q5). Cross-project forks are forbidden.
	ErrCrossProjectFork = errors.New("session_turns: fork from a different project is not allowed")

	// ErrTargetTurnNotFound is returned by ForkSession / RewindSession
	// when the requested at_turn_id / parent_turn_id does not exist in
	// the database. Callers must surface this as a 4xx, not a 5xx.
	ErrTargetTurnNotFound = errors.New("session_turns: target turn not found")

	// ErrEmptySession is returned by ForkSession / RewindSession when
	// the source session has no turns to clone. Defensive: a target turn
	// existing implies the session has at least one turn, so this fires
	// only on data-integrity edge cases (e.g., broken parent chain).
	ErrEmptySession = errors.New("session_turns: cannot fork from an empty session")

	// ErrInvalidRewindMode is returned by RewindSession when the Mode
	// field is set to a value other than "branch" or "truncate", OR
	// when Mode is "truncate" in PR2 (truncate is reserved for PR4 per
	// locked-in decision Q6).
	ErrInvalidRewindMode = errors.New("session_turns: rewind mode must be 'branch' or 'truncate'")

	// ErrTruncateRequiresConfirmation is returned by RewindSession when
	// Mode is "truncate" but ConfirmTruncate=false. PR4 / REQ-011 / Risk
	// #2: truncate is a HIGH-SEVERITY operation and MUST be opt-in. The
	// dedicated sentinel lets callers (CLI, MCP, future audit pipeline)
	// distinguish the opt-in rejection from generic rewind errors via
	// errors.Is — which is the only durable signal a hard-coded string
	// match cannot give across renames / l10n.
	ErrTruncateRequiresConfirmation = errors.New("session_turns: rewind truncate mode requires explicit ConfirmTruncate=true")
)

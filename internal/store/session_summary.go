package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ─── SessionSummaryProjector ──────────────────────────────────────────────────
//
// Implements the PR3 rewire of mem_session_summary (REQ-008). The projector
// reads session_turns leaves per locked-in decision B (note B in design §1.2):
//
//   "Most-recent created_at = the branch the user was last working on.
//    Tie-breaker: lexicographically greatest id (ULIDs sort by time + entropy)."
//
// External contract for callers:
//
//   - ProjectSessionSummary returns the rendered summary text + metadata
//     for a given session. The metadata carries enough information for
//     mem_context (REQ-009) to render the last K turns without re-querying.
//   - CountTurns exposes the per-project counter required by REQ-015.
//
// Fallback behavior (REQ-008 edge case):
//
//   - When the session has NO leaves (no session_turns rows after excluding
//     pre_tree), but a v6 sessions.summary exists, the v6 row is returned
//     verbatim with metadata.v6_fallback=true. This is the honest "degraded
//     mode" for sessions that existed pre-tree and never received a turn
//     save (e.g. only ever had a summary written via the legacy path).
//
//   - When neither leaves nor v6 summary exist, ErrEmptySession is returned.

// SessionTreeSummary is the projector output for a single session. Text is the
// concatenated text blocks from the active leaf's content_json. Metadata
// carries the leaf pointer, tree depth, and turn count so callers (mem_context,
// CLI show, ops dashboards) can render without re-querying.
//
// Named with the "Tree" suffix to avoid colliding with the existing
// store.SessionTreeSummary (the sessions-list view in store.go used by
// RecentSessions / AllSessions).
type SessionTreeSummary struct {
	SessionID string         `json:"session_id"`
	Project   string         `json:"project"`
	Text      string         `json:"text"`
	Metadata  map[string]any `json:"metadata"`
}

// CountTurns returns the total number of session_turns rows for a project,
// excluding pre_tree=true rows by default (Q1 / REQ-005). REQ-015 exposes
// this as the session_turn_count counter; the metric hook in SaveTurn
// emits the per-project increment.
func (s *Store) CountTurns(ctx context.Context, project string) (int, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return 0, ErrProjectRequired
	}
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM session_turns
		WHERE project = ?
		  AND (metadata_json IS NULL
		       OR json_extract(metadata_json, '$.pre_tree') IS NULL
		       OR json_extract(metadata_json, '$.pre_tree') != 1)
	`, project).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("session_summary: CountTurns: %w", err)
	}
	return count, nil
}

// LastKTurns returns up to K most-recent turns for a session, ordered by
// turn_seq ASC (so the output reads chronologically). REQ-009: powers the
// opt-in mem_context.include_last_k_turns. pre_tree rows are excluded by
// default (Q1); includeLegacy=true forces them in.
//
// K <= 0 returns nil without querying (the mem_context path treats it as
// "feature off").
func (s *Store) LastKTurns(ctx context.Context, sessionID, project string, k int, includeLegacy bool) ([]Turn, error) {
	if k <= 0 {
		return nil, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	project = strings.TrimSpace(project)
	if sessionID == "" {
		return nil, fmt.Errorf("session_summary: LastKTurns: session_id is required")
	}
	if project == "" {
		return nil, ErrProjectRequired
	}

	// Subquery: pick the top-K turn_seq values for the session, then
	// re-join for full row data. This keeps the LIMIT + ORDER BY off the
	// full scan and aligns with the (session_id, turn_seq) index.
	query := `
		SELECT t.id, t.session_id, t.project, t.parent_turn_id, t.turn_seq, t.role,
		       t.content_json, t.agent_name, t.tokens_in, t.tokens_out,
		       t.created_at, t.metadata_json
		FROM session_turns t
		JOIN (
			SELECT turn_seq FROM session_turns
			WHERE session_id = ? AND project = ?
			`
	args := []any{sessionID, project}
	if !includeLegacy {
		query += ` AND (metadata_json IS NULL
		            OR json_extract(metadata_json, '$.pre_tree') IS NULL
		            OR json_extract(metadata_json, '$.pre_tree') != 1)`
	}
	query += `
			ORDER BY turn_seq DESC
			LIMIT ?
		) latest ON latest.turn_seq = t.turn_seq AND t.session_id = ? AND t.project = ?
		ORDER BY t.turn_seq ASC
	`
	args = append(args, k, sessionID, project)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("session_summary: LastKTurns: %w", err)
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
		return nil, fmt.Errorf("session_summary: LastKTurns rows: %w", err)
	}
	return out, nil
}

// ProjectSessionSummary builds a SessionTreeSummary from the latest leaf of a
// session's turn tree (REQ-008 + locked-in decision B). When the session
// has no turns but a v6 sessions.summary exists, the v6 row is returned
// verbatim and metadata.v6_fallback is set to true.
func (s *Store) ProjectSessionSummary(ctx context.Context, sessionID, project string) (SessionTreeSummary, error) {
	sessionID = strings.TrimSpace(sessionID)
	project = strings.TrimSpace(project)
	if sessionID == "" {
		return SessionTreeSummary{}, fmt.Errorf("session_summary: session_id is required")
	}
	if project == "" {
		return SessionTreeSummary{}, ErrProjectRequired
	}

	leaf, err := s.activeLeaf(ctx, sessionID, project)
	if err != nil {
		return SessionTreeSummary{}, err
	}

	if leaf == nil {
		// Fallback path: no leaves in the tree. Try v6 sessions.summary.
		return s.v6FallbackSummary(ctx, sessionID, project)
	}

	text := renderLeafText(leaf.ContentJSON)
	depth, _ := s.treeDepth(ctx, sessionID, project)
	turnCount, _ := s.CountTurns(ctx, project)

	summary := SessionTreeSummary{
		SessionID: sessionID,
		Project:   project,
		Text:      text,
		Metadata: map[string]any{
			"leaf_turn_id":  leaf.ID,
			"leaf_turn_seq": leaf.TurnSeq,
			"tree_depth":    depth,
			"turn_count":    turnCount,
			"source":        "session_turns_leaf",
		},
	}
	return summary, nil
}

// ActiveLeaf returns the most-recently-created leaf of a session's turn
// tree, or (Turn{}, false, nil) when the session has no leaves. Exposed
// separately so callers (CLI, mem_context opt-in) can pin to the same leaf
// the projector would pick.
func (s *Store) ActiveLeaf(ctx context.Context, sessionID, project string) (Turn, bool, error) {
	leaf, err := s.activeLeaf(ctx, sessionID, project)
	if err != nil {
		return Turn{}, false, err
	}
	if leaf == nil {
		return Turn{}, false, nil
	}
	return *leaf, true, nil
}

// ─── internals ────────────────────────────────────────────────────────────────

// activeLeaf returns the leaf with the greatest created_at; tie-break on
// lexicographically greatest id (ULIDs sort by time + entropy). Returns
// (nil, nil) when no leaves exist (caller decides fallback).
//
// "Leaf" here = a turn whose parent_turn_id has no row in session_turns
// pointing to it as parent, OR a turn that itself has no parent (a single-
// turn session is trivially its own leaf). The recursive CTE in
// findLeaves implements the lookup.
func (s *Store) activeLeaf(ctx context.Context, sessionID, project string) (*Turn, error) {
	leaves, err := s.findLeaves(ctx, sessionID, project)
	if err != nil {
		return nil, err
	}
	if len(leaves) == 0 {
		return nil, nil
	}
	// Sort by (created_at DESC, id DESC). ULIDs are time-prefixed so id DESC
	// is a deterministic tie-break.
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].CreatedAt != leaves[j].CreatedAt {
			return leaves[i].CreatedAt > leaves[j].CreatedAt
		}
		return leaves[i].ID > leaves[j].ID
	})
	return &leaves[0], nil
}

// findLeaves returns every turn in the session whose turn is NOT referenced
// as a parent_turn_id by any other turn in the same session. A "leaf" in
// this context = the active end of a turn chain. Multiple leaves indicate
// branches (e.g., after a fork).
//
// pre_tree rows are excluded by default — they are synthetic, not user-
// authored turns, and must never be picked as the "active" leaf.
func (s *Store) findLeaves(ctx context.Context, sessionID, project string) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.session_id, t.project, t.parent_turn_id, t.turn_seq, t.role,
		       t.content_json, t.agent_name, t.tokens_in, t.tokens_out,
		       t.created_at, t.metadata_json
		FROM session_turns t
		WHERE t.session_id = ?
		  AND t.project = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM session_turns c
		      WHERE c.parent_turn_id = t.id
		  )
		  AND (t.metadata_json IS NULL
		       OR json_extract(t.metadata_json, '$.pre_tree') IS NULL
		       OR json_extract(t.metadata_json, '$.pre_tree') != 1)
		ORDER BY t.created_at DESC, t.id DESC
	`, sessionID, project)
	if err != nil {
		return nil, fmt.Errorf("session_summary: findLeaves: %w", err)
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
		return nil, fmt.Errorf("session_summary: findLeaves rows: %w", err)
	}
	return out, nil
}

// treeDepth returns the longest root→leaf path length in the session, in
// turns. A 1-turn session has depth=1. A linear 5-turn session has
// depth=5. A forked session takes the longest branch. Used as metadata
// only; cost is O(N) via a recursive CTE bounded by the session row count.
func (s *Store) treeDepth(ctx context.Context, sessionID, project string) (int, error) {
	var rowCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_turns WHERE session_id = ? AND project = ?`,
		sessionID, project,
	).Scan(&rowCount); err != nil {
		return 0, fmt.Errorf("session_summary: treeDepth count: %w", err)
	}
	if rowCount == 0 {
		return 0, nil
	}

	var depth sql.NullInt64
	cte := fmt.Sprintf(`
		WITH RECURSIVE chain(id, depth) AS (
			SELECT id, 1 FROM session_turns
			WHERE session_id = ? AND project = ?
			UNION ALL
			SELECT t.parent_turn_id, c.depth + 1
			FROM session_turns t JOIN chain c ON t.id = c.id
			WHERE t.parent_turn_id IS NOT NULL
			  AND c.depth < %d
		)
		SELECT MAX(depth) FROM chain
	`, rowCount+1)
	if err := s.db.QueryRowContext(ctx, cte, sessionID, project).Scan(&depth); err != nil {
		return 0, fmt.Errorf("session_summary: treeDepth cte: %w", err)
	}
	if !depth.Valid {
		return 0, nil
	}
	return int(depth.Int64), nil
}

// renderLeafText concatenates the text from every text-typed block in a
// leaf's content_json, in order. Non-text blocks (tool-call, tool-result,
// reasoning) are skipped — they aren't natural summary material, and
// embedding them as text would be misleading.
//
// Malformed content_json returns an empty string rather than failing the
// whole projector call: a turn with bad JSON shouldn't take down the
// summary reader.
func renderLeafText(contentJSON []byte) string {
	if len(contentJSON) == 0 {
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(contentJSON, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && strings.TrimSpace(blk.Text) != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// v6FallbackSummary returns the legacy sessions.summary verbatim with
// metadata.v6_fallback=true. Called when no leaves exist in the turn tree.
// This is the degraded mode for sessions that pre-date the v7 migration
// and never received a turn save via the new path.
func (s *Store) v6FallbackSummary(ctx context.Context, sessionID, project string) (SessionTreeSummary, error) {
	var summaryText *string
	err := s.db.QueryRowContext(ctx,
		`SELECT summary FROM sessions WHERE id = ?`, sessionID,
	).Scan(&summaryText)
	if err != nil {
		// sql.ErrNoRows is not an empty session — caller must distinguish.
		// For the projector, an absent session is ErrEmptySession so the
		// caller can return a useful message.
		return SessionTreeSummary{}, fmt.Errorf("session_summary: v6 fallback lookup: %w", err)
	}
	text := ""
	if summaryText != nil {
		text = *summaryText
	}
	if text == "" {
		// No v6 summary either: the session is genuinely empty.
		return SessionTreeSummary{}, fmt.Errorf("session_summary: session %q has no turns and no summary: %w",
			sessionID, ErrEmptySession)
	}
	return SessionTreeSummary{
		SessionID: sessionID,
		Project:   project,
		Text:      text,
		Metadata: map[string]any{
			"v6_fallback": true,
			"source":      "sessions.summary",
		},
	}, nil
}

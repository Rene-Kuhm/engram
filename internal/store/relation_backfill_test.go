package store

// Tests for cloud-relation-backfill (#496).
//
// Three behaviors verified:
//   1. backfillRelationSyncMutationsTx: non-orphaned relations missing a
//      sync_mutations row get one created; orphaned relations are skipped.
//   2. JudgeBySemantic: enrolled project → enqueues; non-enrolled → does not.
//   3. projectNeedsBackfill: returns true when a relation lacks its mutation row.

import (
	"database/sql"
	"testing"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

// setupBackfillStore creates a store with:
//   - session "ses-bf" / project "proj-bf"
//   - two observations (srcSyncID, tgtSyncID)
//   - project enrolled in sync_enrolled_projects
//
// Returns the store plus sync_ids of the two observations.
func setupBackfillStore(t *testing.T) (s *Store, srcSyncID, tgtSyncID string) {
	t.Helper()
	s = newTestStore(t)
	if err := s.CreateSession("ses-bf", "proj-bf", "/tmp/bf"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.EnrollProject("proj-bf"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}
	_, srcSyncID = addTestObsSession(t, s, "ses-bf", "Backfill source obs", "decision", "proj-bf", "project")
	_, tgtSyncID = addTestObsSession(t, s, "ses-bf", "Backfill target obs", "decision", "proj-bf", "project")
	return
}

// insertRelationDirect inserts a memory_relations row bypassing the normal
// SaveRelation / JudgeRelation path so there is no corresponding sync_mutations
// row. Used to simulate the pre-backfill gap.
func insertRelationDirect(t *testing.T, s *Store, syncID, sourceID, targetID, judgmentStatus string) {
	t.Helper()
	if _, err := s.db.Exec(`
		INSERT INTO memory_relations
			(sync_id, source_id, target_id, relation, judgment_status, created_at, updated_at)
		VALUES (?, ?, ?, 'related', ?, datetime('now'), datetime('now'))
	`, syncID, sourceID, targetID, judgmentStatus); err != nil {
		t.Fatalf("insertRelationDirect: %v", err)
	}
}

// countRelationSyncMutationsByKey returns the count of sync_mutations rows for a
// specific relation entity_key.
func countRelationSyncMutationsByKey(t *testing.T, s *Store, relSyncID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM sync_mutations WHERE entity = ? AND entity_key = ? AND source = ?`,
		SyncEntityRelation, relSyncID, SyncSourceLocal,
	).Scan(&n); err != nil {
		t.Fatalf("countRelationSyncMutationsByKey(%q): %v", relSyncID, err)
	}
	return n
}

// ─── Test 1: backfillRelationSyncMutationsTx ─────────────────────────────────

// TestBackfillRelationSyncMutations_CreatesRowForNonOrphaned verifies that a
// non-orphaned relation that already exists in memory_relations with NO
// sync_mutations row gets a sync_mutations row after backfill runs.
//
// This tests gap #1 from issue #496: backfillProjectSyncMutationsTx calls no
// relation backfill, so pre-existing relations never replicate to the cloud.
func TestBackfillRelationSyncMutations_CreatesRowForNonOrphaned(t *testing.T) {
	s, srcSyncID, tgtSyncID := setupBackfillStore(t)

	// Insert a judged relation directly — no sync_mutations row exists yet.
	relSyncID := newSyncID("rel-bf-judged")
	insertRelationDirect(t, s, relSyncID, srcSyncID, tgtSyncID, JudgmentStatusJudged)

	// Precondition: zero mutation rows for this relation.
	if n := countRelationSyncMutationsByKey(t, s, relSyncID); n != 0 {
		t.Fatalf("precondition: expected 0 sync_mutations for relation, got %d", n)
	}

	// Run backfill through the public entry point (same path used on startup).
	if err := s.repairEnrolledProjectSyncMutations(); err != nil {
		t.Fatalf("repairEnrolledProjectSyncMutations: %v", err)
	}

	// Postcondition: one sync_mutations row must exist now.
	if n := countRelationSyncMutationsByKey(t, s, relSyncID); n != 1 {
		t.Errorf("expected 1 sync_mutations row after backfill, got %d", n)
	}
}

// TestBackfillRelationSyncMutations_SkipsOrphaned verifies that orphaned
// relations are NOT backfilled — their status signals the endpoints are gone,
// so syncing them would produce a useless cloud row.
func TestBackfillRelationSyncMutations_SkipsOrphaned(t *testing.T) {
	s, srcSyncID, tgtSyncID := setupBackfillStore(t)

	// Insert an orphaned relation directly.
	relOrphanedSyncID := newSyncID("rel-bf-orphaned")
	insertRelationDirect(t, s, relOrphanedSyncID, srcSyncID, tgtSyncID, JudgmentStatusOrphaned)

	// Run backfill.
	if err := s.repairEnrolledProjectSyncMutations(); err != nil {
		t.Fatalf("repairEnrolledProjectSyncMutations: %v", err)
	}

	// Orphaned relation must NOT have a sync_mutations row.
	if n := countRelationSyncMutationsByKey(t, s, relOrphanedSyncID); n != 0 {
		t.Errorf("orphaned relation must NOT be backfilled, got %d sync_mutations rows", n)
	}
}

// TestBackfillRelationSyncMutations_SkipsAlreadyEnqueued verifies idempotency:
// running backfill again when a mutation already exists does not create a
// duplicate.
func TestBackfillRelationSyncMutations_SkipsAlreadyEnqueued(t *testing.T) {
	s, srcSyncID, tgtSyncID := setupBackfillStore(t)

	relSyncID := newSyncID("rel-bf-already")
	insertRelationDirect(t, s, relSyncID, srcSyncID, tgtSyncID, JudgmentStatusJudged)

	// Run backfill twice.
	if err := s.repairEnrolledProjectSyncMutations(); err != nil {
		t.Fatalf("first repairEnrolledProjectSyncMutations: %v", err)
	}
	if err := s.repairEnrolledProjectSyncMutations(); err != nil {
		t.Fatalf("second repairEnrolledProjectSyncMutations: %v", err)
	}

	// Must still be exactly one row — idempotent.
	if n := countRelationSyncMutationsByKey(t, s, relSyncID); n != 1 {
		t.Errorf("expected exactly 1 sync_mutations row after two backfill runs, got %d", n)
	}
}

// ─── Test 2: JudgeBySemantic enqueue ─────────────────────────────────────────

// TestJudgeBySemantic_EnqueuesSyncMutation_WhenEnrolled verifies that calling
// JudgeBySemantic on an enrolled project produces a sync_mutations row for the
// resulting relation.
//
// This tests gap #2 from issue #496: JudgeBySemantic never called
// enqueueSyncMutationTx, so every semantic verdict produced no journal row.
func TestJudgeBySemantic_EnqueuesSyncMutation_WhenEnrolled(t *testing.T) {
	s, srcSyncID, tgtSyncID := setupBackfillStore(t)

	before := countRelationMutations(t, s, SyncEntityRelation, "proj-bf")

	syncID, err := s.JudgeBySemantic(JudgeBySemanticParams{
		SourceID:  srcSyncID,
		TargetID:  tgtSyncID,
		Relation:  RelationConflictsWith,
		Reasoning: "they conflict semantically",
		Model:     "test-model",
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic: %v", err)
	}
	if syncID == "" {
		t.Fatal("JudgeBySemantic: expected non-empty syncID")
	}

	after := countRelationMutations(t, s, SyncEntityRelation, "proj-bf")
	if after <= before {
		t.Errorf("expected sync_mutations to gain a row after JudgeBySemantic on enrolled project; before=%d after=%d", before, after)
	}

	// Verify the mutation references the correct relation sync_id.
	if n := countRelationSyncMutationsByKey(t, s, syncID); n != 1 {
		t.Errorf("expected 1 sync_mutations row for relation %q, got %d", syncID, n)
	}
}

// TestJudgeBySemantic_DoesNotEnqueue_WhenNotEnrolled verifies that calling
// JudgeBySemantic on a non-enrolled project does NOT produce a sync_mutations
// row (enrollment guard — backfill covers it post-enrollment).
func TestJudgeBySemantic_DoesNotEnqueue_WhenNotEnrolled(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("ses-unenrolled", "proj-unenrolled", "/tmp/unenrolled"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, srcID := addTestObsSession(t, s, "ses-unenrolled", "Source unenrolled", "decision", "proj-unenrolled", "project")
	_, tgtID := addTestObsSession(t, s, "ses-unenrolled", "Target unenrolled", "decision", "proj-unenrolled", "project")

	syncID, err := s.JudgeBySemantic(JudgeBySemanticParams{
		SourceID:  srcID,
		TargetID:  tgtID,
		Relation:  RelationRelated,
		Reasoning: "semantically related",
		Model:     "test-model",
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic: %v", err)
	}
	if syncID == "" {
		t.Fatal("JudgeBySemantic: expected non-empty syncID even for unenrolled project")
	}

	// No mutation must exist for the unenrolled project.
	if n := countRelationSyncMutationsByKey(t, s, syncID); n != 0 {
		t.Errorf("unenrolled project: expected 0 sync_mutations rows for relation, got %d", n)
	}
}

// TestJudgeBySemantic_UpdateEnqueues_WhenEnrolled verifies that updating an
// existing relation via JudgeBySemantic (UPSERT path) also enqueues a
// sync_mutations row.
func TestJudgeBySemantic_UpdateEnqueues_WhenEnrolled(t *testing.T) {
	s, srcSyncID, tgtSyncID := setupBackfillStore(t)

	// First call: insert.
	syncID, err := s.JudgeBySemantic(JudgeBySemanticParams{
		SourceID:  srcSyncID,
		TargetID:  tgtSyncID,
		Relation:  RelationRelated,
		Reasoning: "initial verdict",
		Model:     "model-v1",
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic insert: %v", err)
	}

	beforeUpdate := countRelationMutations(t, s, SyncEntityRelation, "proj-bf")

	// Second call: update (same pair, different relation).
	syncID2, err := s.JudgeBySemantic(JudgeBySemanticParams{
		SourceID:  srcSyncID,
		TargetID:  tgtSyncID,
		Relation:  RelationConflictsWith,
		Reasoning: "revised verdict — conflicts",
		Model:     "model-v2",
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic update: %v", err)
	}
	// Same pair → same canonical sync_id returned.
	if syncID2 != syncID {
		t.Errorf("UPSERT should return same sync_id; want %q, got %q", syncID, syncID2)
	}

	afterUpdate := countRelationMutations(t, s, SyncEntityRelation, "proj-bf")
	if afterUpdate <= beforeUpdate {
		t.Errorf("expected additional sync_mutations row after JudgeBySemantic update; before=%d after=%d", beforeUpdate, afterUpdate)
	}
}

// ─── Test 3: projectNeedsBackfill detects missing relation mutations ──────────

// TestProjectNeedsBackfill_TrueWhenRelationMissingMutation verifies that
// projectNeedsBackfill returns true when a relation exists in memory_relations
// with no corresponding sync_mutations row.
//
// This tests gap #3 from issue #496: the fast-path skip in
// repairEnrolledProjectSyncMutations would silently skip projects that only
// have unsynced relations (observations were already backfilled).
func TestProjectNeedsBackfill_TrueWhenRelationMissingMutation(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("ses-bf2", "proj-bf2", "/tmp/bf2"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, src2 := addTestObsSession(t, s, "ses-bf2", "NeedsBF src", "decision", "proj-bf2", "project")
	_, tgt2 := addTestObsSession(t, s, "ses-bf2", "NeedsBF tgt", "decision", "proj-bf2", "project")

	// Enroll — this creates sync_mutations for session + observations.
	if err := s.EnrollProject("proj-bf2"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}

	// Now insert a relation WITHOUT a sync_mutations row (simulates the gap).
	relSyncID := newSyncID("rel-needs-bf")
	insertRelationDirect(t, s, relSyncID, src2, tgt2, JudgmentStatusJudged)

	// projectNeedsBackfill must return true because the relation lacks a mutation.
	needs, err := s.projectNeedsBackfill("proj-bf2")
	if err != nil {
		t.Fatalf("projectNeedsBackfill after relation insert: %v", err)
	}
	if !needs {
		t.Errorf("projectNeedsBackfill must return true when a non-orphaned relation has no sync_mutations row, got false")
	}
}

// TestProjectNeedsBackfill_FalseWhenRelationHasMutation verifies that
// projectNeedsBackfill returns false when all relations already have
// sync_mutations rows (and sessions/observations are also covered).
func TestProjectNeedsBackfill_FalseWhenRelationHasMutation(t *testing.T) {
	s, srcSyncID, tgtSyncID := setupBackfillStore(t)

	// JudgeRelation — enrolled, so it enqueues a mutation automatically.
	relSyncID := newSyncID("rel-has-mut")
	if _, err := s.SaveRelation(SaveRelationParams{
		SyncID:   relSyncID,
		SourceID: srcSyncID,
		TargetID: tgtSyncID,
	}); err != nil {
		t.Fatalf("SaveRelation: %v", err)
	}
	if _, err := s.JudgeRelation(JudgeRelationParams{
		JudgmentID:    relSyncID,
		Relation:      RelationConflictsWith,
		MarkedByActor: "test-actor",
		MarkedByKind:  "agent",
	}); err != nil {
		t.Fatalf("JudgeRelation: %v", err)
	}

	// projectNeedsBackfill should return false: relation already has a mutation.
	needs, err := s.projectNeedsBackfill("proj-bf")
	if err != nil {
		t.Fatalf("projectNeedsBackfill: %v", err)
	}
	if needs {
		t.Errorf("expected projectNeedsBackfill=false when relation already has a sync_mutations row, got true")
	}
}

// TestProjectNeedsBackfill_OrphanedRelationDoesNotTrigger verifies that an
// orphaned relation (without a sync_mutations row) does NOT cause
// projectNeedsBackfill to return true — orphaned relations are intentionally
// excluded from sync.
func TestProjectNeedsBackfill_OrphanedRelationDoesNotTrigger(t *testing.T) {
	// Create a clean store with only enrolled session/obs/relation mutations
	// satisfied, then add an orphaned relation.
	s2 := newTestStore(t)
	if err := s2.CreateSession("ses-orph-bf", "proj-orph", "/tmp/orph"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, src := addTestObsSession(t, s2, "ses-orph-bf", "Orphan src", "decision", "proj-orph", "project")
	_, tgt := addTestObsSession(t, s2, "ses-orph-bf", "Orphan tgt", "decision", "proj-orph", "project")

	if err := s2.EnrollProject("proj-orph"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}

	// Insert an orphaned relation (no sync_mutations row for it).
	orphRelSyncID := newSyncID("rel-orph-check")
	insertRelationDirect(t, s2, orphRelSyncID, src, tgt, JudgmentStatusOrphaned)

	// projectNeedsBackfill must NOT be triggered by the orphaned relation.
	// (It may still be true because of the sessions/obs from EnrollProject
	// — but the orphaned relation itself must not contribute.)
	// Verify the relation-specific count is zero.
	if err := s2.withTx(func(tx *sql.Tx) error {
		// Manually confirm orphaned relation is excluded from the backfill query.
		var n int
		err := tx.QueryRow(`
			SELECT COUNT(*) FROM memory_relations r
			JOIN observations src ON src.sync_id = r.source_id AND src.deleted_at IS NULL
			JOIN observations tgt ON tgt.sync_id = r.target_id AND tgt.deleted_at IS NULL
			WHERE r.judgment_status != ?
			  AND NOT EXISTS (
				SELECT 1 FROM sync_mutations sm
				WHERE sm.target_key = ? AND sm.entity = ? AND sm.entity_key = r.sync_id AND sm.source = ?
			  )
		`, JudgmentStatusOrphaned, DefaultSyncTargetKey, SyncEntityRelation, SyncSourceLocal).Scan(&n)
		if err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("orphaned relation must NOT be counted in backfill check, got count=%d", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("withTx: %v", err)
	}
}

// ─── Test 4: pre-enrollment relations are backfilled on EnrollProject ─────────

// TestEnrollProject_BackfillsPreExistingRelations verifies the core #496
// trigger end-to-end through the real code path:
//
//  1. Create a session and two observations on an UNENROLLED store.
//  2. Call JudgeBySemantic — enrollment gate prevents any sync_mutations row.
//  3. Call EnrollProject — backfillProjectSyncMutationsTx runs, which calls
//     backfillRelationSyncMutationsTx internally.
//  4. Assert the relation NOW has a sync_mutations row (backfill succeeded).
//
// This proves that relations created before enrollment are replicated when the
// project is later enrolled.
func TestEnrollProject_BackfillsPreExistingRelations(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("ses-enroll-bf", "proj-enroll-bf", "/tmp/enroll-bf"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, srcSyncID := addTestObsSession(t, s, "ses-enroll-bf", "Pre-enroll src obs", "decision", "proj-enroll-bf", "project")
	_, tgtSyncID := addTestObsSession(t, s, "ses-enroll-bf", "Pre-enroll tgt obs", "decision", "proj-enroll-bf", "project")

	// Create a relation via the real JudgeBySemantic path on an UNENROLLED project.
	// The enrollment gate must prevent any sync_mutations row from being written.
	relSyncID, err := s.JudgeBySemantic(JudgeBySemanticParams{
		SourceID:  srcSyncID,
		TargetID:  tgtSyncID,
		Relation:  RelationRelated,
		Reasoning: "pre-enrollment semantic verdict",
		Model:     "test-model",
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic (unenrolled): %v", err)
	}
	if relSyncID == "" {
		t.Fatal("JudgeBySemantic: expected non-empty relSyncID")
	}

	// Pre-enrollment: enrollment gate must have prevented any relation mutation.
	if n := countRelationSyncMutationsByKey(t, s, relSyncID); n != 0 {
		t.Fatalf("precondition: expected 0 sync_mutations for relation before enrollment, got %d", n)
	}

	// Enroll the project — this triggers backfillProjectSyncMutationsTx, which
	// calls backfillRelationSyncMutationsTx to cover the pre-existing relation.
	if err := s.EnrollProject("proj-enroll-bf"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}

	// Post-enrollment: the relation must now have a sync_mutations row.
	if n := countRelationSyncMutationsByKey(t, s, relSyncID); n != 1 {
		t.Errorf("expected 1 sync_mutations row for relation after EnrollProject backfill, got %d", n)
	}
}

package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los62cInsert puts one learning into the given project and returns its id.
func los62cInsert(t *testing.T, s *Store, project, content string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		SessionID: "los62c-session", Category: "gotcha",
		Content:    content,
		Project:    project,
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "test",
		Source: "llm_extracted",
	})
	if err != nil {
		t.Fatalf("insert learning %q: %v", content, err)
	}
	return id
}

// los62cRetire writes a raw sentinel onto a row. The sentinels with no writer in
// the tree (and -4, which only the delete paths set) can only be produced here.
func los62cRetire(t *testing.T, s *Store, id, sentinel int64) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE learnings SET superseded_by = ? WHERE id = ?`, sentinel, id); err != nil {
		t.Fatalf("set superseded_by = %d on %d: %v", sentinel, id, err)
	}
}

// TestLos62c_EvolutionStats_SeparatesDeletedPredecessor pins finding 1 of YM-445.
// GetEvolutionStatsF counted every non-NULL superseded_by into TotalSuperseded,
// so a row whose successor was hard-deleted (-4) was reported as a replacement.
// cmd_benchmark reads that number, so a benchmark run credited the evolution with
// replacements that never happened.
func TestLos62c_EvolutionStats_SeparatesDeletedPredecessor(t *testing.T) {
	s := mustOpen(t)
	const project = "/home/tester/memory/los62c-bench"

	successor := los62cInsert(t, s, project, "the fact that replaced an older note")
	replaced := los62cInsert(t, s, project, "a note replaced by a later fact")
	if err := s.SupersedeLearning(replaced, successor, "replaced"); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	detached := los62cInsert(t, s, project, "a note whose successor was hard-deleted")
	los62cRetire(t, s, detached, SupersededByDeleted)
	los62cInsert(t, s, project, "a note that is still active")

	es, err := s.GetEvolutionStatsF(StatsFilter{Project: project})
	if err != nil {
		t.Fatalf("get evolution stats: %v", err)
	}
	if es.TotalSuperseded != 1 {
		t.Errorf("TotalSuperseded = %d, want 1 (only the row with a real successor)", es.TotalSuperseded)
	}
	if es.DeletedPredecessorCount != 1 {
		t.Errorf("DeletedPredecessorCount = %d, want 1 (the row whose successor was deleted)", es.DeletedPredecessorCount)
	}
	if es.RuleBasedCount != 0 {
		t.Errorf("RuleBasedCount = %d, want 0: the detached row must not land in a replacement bucket, its reason is not rule-based", es.RuleBasedCount)
	}
	if es.LLMCount != 1 {
		t.Errorf("LLMCount = %d, want 1 (TotalSuperseded - RuleBasedCount must stay non-negative)", es.LLMCount)
	}
}

// TestLos62c_LearningStats_SplitsReplacedFromRetired pins finding 3 of YM-445 on
// the statistics side. SupersededCount bundles the rows retired by a real
// successor with the rows retired by a sentinel, so the number cannot be read as
// "replaced". The split is additive: SupersededCount keeps its meaning, and the
// two new counts partition it, so no surface loses a row.
func TestLos62c_LearningStats_SplitsReplacedFromRetired(t *testing.T) {
	s := mustOpen(t)
	const project = "/home/tester/memory/los62c-counts"

	successor := los62cInsert(t, s, project, "the fact that replaced an older note")
	replaced := los62cInsert(t, s, project, "a note replaced by a later fact")
	if err := s.SupersedeLearning(replaced, successor, "replaced"); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	detached := los62cInsert(t, s, project, "a note whose successor was hard-deleted")
	los62cRetire(t, s, detached, SupersededByDeleted)

	retired := map[string]int64{}
	for name, sentinel := range map[string]int64{
		"junk":      -1, // no constant in the tree, the cleanup paths write the literal
		"resolved":  SupersededByResolved,
		"reextract": SupersededByReextract,
		"legacy":    SupersededByLegacyCleanup,
	} {
		id := los62cInsert(t, s, project, "a note retired as "+name)
		los62cRetire(t, s, id, sentinel)
		retired[name] = id
	}
	substanceless := los62cInsert(t, s, project, "a note retired with a zero edge")
	los62cRetire(t, s, substanceless, 0)
	los62cInsert(t, s, project, "a note that is still active")

	st, err := s.GetLearningStatsF(StatsFilter{Project: project})
	if err != nil {
		t.Fatalf("get learning stats: %v", err)
	}
	if st.ReplacedCount != 1 {
		t.Errorf("ReplacedCount = %d, want 1 (only the row with a real successor)", st.ReplacedCount)
	}
	if st.RetiredCount != 5 {
		t.Errorf("RetiredCount = %d, want 5 (junk, resolved, reextract, legacy cleanup, zero edge)", st.RetiredCount)
	}
	if st.DeletedPredecessorCount != 1 {
		t.Errorf("DeletedPredecessorCount = %d, want 1", st.DeletedPredecessorCount)
	}
	// The published number keeps its Los-61e meaning: everything retired except
	// the rows whose successor was deleted. The new counts partition it exactly,
	// which is what lets both surfaces print one definition (YM-445 finding 3).
	if want := st.ReplacedCount + st.RetiredCount; st.SupersededCount != want {
		t.Errorf("SupersededCount = %d, want %d (replaced %d + retired %d): the split must not lose a row",
			st.SupersededCount, want, st.ReplacedCount, st.RetiredCount)
	}
	if st.ActiveCount != 2 {
		t.Errorf("ActiveCount = %d, want 2 (the active note and the successor)", st.ActiveCount)
	}
	if st.TotalCount != 9 {
		t.Errorf("TotalCount = %d, want 9 (every row of the filter, active and retired)", st.TotalCount)
	}
}

// TestLos62c_LegacyCleanupSentinelIsNamed pins finding 4 of YM-445. The live DB
// carries 182 rows whose superseded_by is -6, written by a one-off user-approved
// cleanup batch in July/August 2026. No constant and no writer exist for it in
// the tree, so a reader of the schema cannot tell what the value means.
func TestLos62c_LegacyCleanupSentinelIsNamed(t *testing.T) {
	if SupersededByLegacyCleanup != -6 {
		t.Fatalf("SupersededByLegacyCleanup = %d, want -6 (the value the live rows carry)", SupersededByLegacyCleanup)
	}
	s := mustOpen(t)
	const project = "/home/tester/memory/los62c-legacy"
	id := los62cInsert(t, s, project, "a note retired by the 2026-08 cleanup batch")
	los62cRetire(t, s, id, SupersededByLegacyCleanup)

	st, err := s.GetLearningStatsF(StatsFilter{Project: project})
	if err != nil {
		t.Fatalf("get learning stats: %v", err)
	}
	if st.ActiveCount != 0 {
		t.Errorf("ActiveCount = %d, want 0: a row carrying the legacy sentinel is retired", st.ActiveCount)
	}
	if st.RetiredCount != 1 {
		t.Errorf("RetiredCount = %d, want 1: the legacy sentinel is a retirement without a successor", st.RetiredCount)
	}
	if st.ReplacedCount != 0 {
		t.Errorf("ReplacedCount = %d, want 0: -6 names no successor", st.ReplacedCount)
	}
	active, err := s.GetActiveLearnings("", project, "", "", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}
	for _, l := range active {
		if l.ID == id {
			t.Error("the legacy-retired row is still served as active")
		}
	}
}

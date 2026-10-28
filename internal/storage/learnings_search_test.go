package storage

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// insertTestLearningWithTime inserts a learning with a specific created_at time and returns its ID.
func insertTestLearningWithTime(t *testing.T, s *Store, content, category string, createdAt time.Time) int64 {
	t.Helper()

	res, err := s.db.Exec(`INSERT INTO learnings(session_id, category, content, created_at, confidence, model_used, source, embedding_content_hash)
		VALUES (?, ?, ?, ?, 1.0, 'test', 'llm_extracted', '')`,
		"test-session", category, content, createdAt.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert test learning: %v", err)
	}
	id, _ := res.LastInsertId()

	// FTS insert so bm25() can find it
	s.db.Exec(`INSERT INTO learnings_fts(rowid, content) VALUES (?, ?)`, id, content)
	return id
}

// insertTestLearningForProject inserts a learning with project and canonical_project set,
// plus the FTS row for bm25() matching. Used for project-filter tests.
func insertTestLearningForProject(t *testing.T, s *Store, content, category, project, canonicalProject string) int64 {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO learnings(session_id, category, content, created_at, confidence, model_used, source, embedding_content_hash, project, canonical_project)
		VALUES (?, ?, ?, ?, 1.0, 'test', 'llm_extracted', '', ?, ?)`,
		"test-session", category, content, time.Now().UTC().Format(time.RFC3339), project, canonicalProject)
	if err != nil {
		t.Fatalf("insert test learning for project: %v", err)
	}
	id, _ := res.LastInsertId()
	s.db.Exec(`INSERT INTO learnings_fts(rowid, content) VALUES (?, ?)`, id, content)
	return id
}

// insertAnticipatedQuery inserts an anticipated query for a learning into both
// the junction table and the FTS virtual table.
func insertAnticipatedQuery(t *testing.T, s *Store, learningID int64, value string) {
	t.Helper()

	_, err := s.db.Exec(`INSERT INTO learning_anticipated_queries (learning_id, value) VALUES (?, ?)`, learningID, value)
	if err != nil {
		t.Fatalf("insert learning_aq: %v", err)
	}
	_, err = s.db.Exec(`INSERT INTO anticipated_queries_fts (value, learning_id) VALUES (?, ?)`, value, learningID)
	if err != nil {
		t.Fatalf("insert aq_fts: %v", err)
	}
}

func TestSearchAnticipatedQueries_NoDateFilter(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()
	oneDayAgo := now.Add(-24 * time.Hour)

	oldID := insertTestLearningWithTime(t, s, "old cache bug in SQLite", "gotcha", oneDayAgo)
	newID := insertTestLearningWithTime(t, s, "new cache bug in SQLite", "gotcha", now.Add(-30*time.Minute))
	insertAnticipatedQuery(t, s, oldID, "cache bug sqlite fix")
	insertAnticipatedQuery(t, s, newID, "cache bug sqlite fix")

	results, err := s.SearchAnticipatedQueries("cache bug sqlite fix", "", 10, "", "")
	if err != nil {
		t.Fatalf("SearchAnticipatedQueries: %v", err)
	}

	foundOld, foundNew := false, false
	for _, r := range results {
		if r.ID == fmt.Sprintf("%d", oldID) {
			foundOld = true
		}
		if r.ID == fmt.Sprintf("%d", newID) {
			foundNew = true
		}
	}
	if !foundOld || !foundNew {
		t.Fatalf("without date filter: expected both old(%d) and new(%d), got old=%v new=%v", oldID, newID, foundOld, foundNew)
	}
}

func TestSearchAnticipatedQueries_SinceFilter(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()
	oneHourAgo := now.Add(-1 * time.Hour)
	oneDayAgo := now.Add(-24 * time.Hour)

	oldID := insertTestLearningWithTime(t, s, "old SSL cert expiry handling", "gotcha", oneDayAgo)
	newID := insertTestLearningWithTime(t, s, "new SSL cert expiry handling", "gotcha", now.Add(-30*time.Minute))
	insertAnticipatedQuery(t, s, oldID, "ssl cert handling")
	insertAnticipatedQuery(t, s, newID, "ssl cert handling")

	since := oneHourAgo.Format(time.RFC3339)
	results, err := s.SearchAnticipatedQueries("ssl cert handling", "", 10, since, "")
	if err != nil {
		t.Fatalf("SearchAnticipatedQueries with since: %v", err)
	}

	for _, r := range results {
		if r.ID == fmt.Sprintf("%d", oldID) {
			t.Fatalf("old learning(%d) created at %s should NOT appear with since=%s", oldID, oneDayAgo.Format(time.RFC3339), since)
		}
	}
	foundNew := false
	for _, r := range results {
		if r.ID == fmt.Sprintf("%d", newID) {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatalf("new learning(%d) created at %s SHOULD appear with since=%s", newID, now.Add(-30*time.Minute).Format(time.RFC3339), since)
	}
}

func TestSearchAnticipatedQueries_BeforeFilter(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()
	oneHourAgo := now.Add(-1 * time.Hour)

	oldID := insertTestLearningWithTime(t, s, "ancient deployment recipe for k8s", "pattern", now.Add(-2*time.Hour))
	newID := insertTestLearningWithTime(t, s, "latest deployment recipe for k8s", "pattern", now)
	insertAnticipatedQuery(t, s, oldID, "deployment recipe k8s")
	insertAnticipatedQuery(t, s, newID, "deployment recipe k8s")

	before := oneHourAgo.Format(time.RFC3339)
	results, err := s.SearchAnticipatedQueries("deployment recipe k8s", "", 10, "", before)
	if err != nil {
		t.Fatalf("SearchAnticipatedQueries with before: %v", err)
	}

	for _, r := range results {
		if r.ID == fmt.Sprintf("%d", newID) {
			t.Fatalf("new learning(%d) created at %s should NOT appear with before=%s", newID, now.Format(time.RFC3339), before)
		}
	}
	foundOld := false
	for _, r := range results {
		if r.ID == fmt.Sprintf("%d", oldID) {
			foundOld = true
		}
	}
	if !foundOld {
		t.Fatalf("old learning(%d) created at %s SHOULD appear with before=%s", oldID, now.Add(-2*time.Hour).Format(time.RFC3339), before)
	}
}

func TestSearchAnticipatedQueries_EmptyWithDateMismatch(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()

	oldID := insertTestLearningWithTime(t, s, "very old redis connection pool fix", "gotcha", now.Add(-7*24*time.Hour))
	insertAnticipatedQuery(t, s, oldID, "redis connection pool fix")

	// Search for a time window where no learnings exist
	since := now.Add(-1 * time.Hour).Format(time.RFC3339)
	results, err := s.SearchAnticipatedQueries("redis connection pool fix", "", 10, since, "")
	if err != nil {
		t.Fatalf("SearchAnticipatedQueries: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for empty window, got %d", len(results))
	}
}

// TestSearchLearningsBM25Ctx_ProjectFilter_FullPathMatches covers the regression from
// the resolve-short-cwd merge: when the caller passes a full path as project filter
// ("/home/user/code/yesmem") but learnings carry short canonical_project ("yesmem"),
// the learning must still match. Before the fix the filter compared canonical_project
// against project via string inequality and filtered everything out.
func TestSearchLearningsBM25Ctx_ProjectFilter_FullPathMatches(t *testing.T) {
	s := mustOpen(t)
	insertTestLearningForProject(t, s, "hybrid search bm25 project filter regression", "gotcha",
		"/home/user/code/yesmem", "yesmem")

	results, err := s.SearchLearningsBM25Ctx(context.Background(), "project filter regression", "/home/user/code/yesmem", "", "", 10)
	if err != nil {
		t.Fatalf("SearchLearningsBM25Ctx: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected BM25 hit when project=/full/path but canonical_project=short-name; got 0 (regression)")
	}
}

// TestSearchLearningsBM25Ctx_ProjectFilter_ShortNameMatches covers the legacy short-name
// caller path: project="yesmem" and canonical_project="yesmem" must keep matching.
func TestSearchLearningsBM25Ctx_ProjectFilter_ShortNameMatches(t *testing.T) {
	s := mustOpen(t)
	insertTestLearningForProject(t, s, "short name hybrid search bm25 match", "gotcha",
		"/home/user/code/yesmem", "yesmem")

	results, err := s.SearchLearningsBM25Ctx(context.Background(), "short name hybrid bm25", "yesmem", "", "", 10)
	if err != nil {
		t.Fatalf("SearchLearningsBM25Ctx: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected BM25 hit when project=canonical=yesmem; got 0")
	}
}

// TestSearchLearningsBM25Ctx_ProjectFilter_WorktreeMatches covers the worktree case:
// project passed as "/home/user/code/yesmem/.worktrees/foo", canonical_project is "yesmem".
// CanonicalProject() strips the worktree suffix so the filter still matches.
func TestSearchLearningsBM25Ctx_ProjectFilter_WorktreeMatches(t *testing.T) {
	s := mustOpen(t)
	insertTestLearningForProject(t, s, "worktree project filter must match parent canonical", "gotcha",
		"/home/user/code/yesmem/.worktrees/foo", "yesmem")

	results, err := s.SearchLearningsBM25Ctx(context.Background(), "worktree project filter parent", "/home/user/code/yesmem/.worktrees/foo", "", "", 10)
	if err != nil {
		t.Fatalf("SearchLearningsBM25Ctx: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected BM25 hit when project=worktree path, canonical_project=parent basename; got 0")
	}
}

// TestSearchLearningsBM25Ctx_ProjectFilter_NoMatchFiltered verifies the filter still
// excludes genuinely unrelated projects (no false positives from tolerance).
func TestSearchLearningsBM25Ctx_ProjectFilter_NoMatchFiltered(t *testing.T) {
	s := mustOpen(t)
	insertTestLearningForProject(t, s, "unrelated project must be filtered out bm25", "gotcha",
		"/home/user/code/yesmem", "yesmem")

	results, err := s.SearchLearningsBM25Ctx(context.Background(), "unrelated project filtered bm25", "/home/user/code/other", "", "", 10)
	if err != nil {
		t.Fatalf("SearchLearningsBM25Ctx: %v", err)
	}
	for _, r := range results {
		if r.Project == "/home/user/code/yesmem" || r.Project == "yesmem" {
			t.Fatalf("expected unrelated project to be filtered, but got result from yesmem: %+v", r)
		}
	}
}

// TestSearchAnticipatedQueries_ProjectFilter_Tolerant covers the AQ path which has
// the same canonical_project != project bug at runAQFTSQuery. Full-path project filter
// must match against short-name canonical_project.
func TestSearchAnticipatedQueries_ProjectFilter_Tolerant(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()
	id := insertTestLearningWithTime(t, s, "aq tolerant project filter for full path", "gotcha", now)
	insertAnticipatedQuery(t, s, id, "aq tolerant project filter")
	// Set canonical_project to short name after insert (insertTestLearningWithTime doesn't set it)
	s.db.Exec(`UPDATE learnings SET canonical_project = 'yesmem', project = '/home/user/code/yesmem' WHERE id = ?`, id)

	results, err := s.SearchAnticipatedQueries("aq tolerant project filter", "/home/user/code/yesmem", 10, "", "")
	if err != nil {
		t.Fatalf("SearchAnticipatedQueries: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected AQ hit with full-path project filter against short canonical_project; got 0 (regression)")
	}
}

// TestSearchLearningsBM25Ctx_ProjectFilter_SameBasenameDifferentParentNotMatch is the
// collision guard called out in Plan Risk #1: /home/a/foo and /home/b/foo both canonicalize
// to "foo", but they are different repos and must NOT match each other. This test failed
// before the both-abs short-circuit was added to projectMatchesTolerant.
func TestSearchLearningsBM25Ctx_ProjectFilter_SameBasenameDifferentParentNotMatch(t *testing.T) {
	s := mustOpen(t)
	insertTestLearningForProject(t, s, "same basename different parent must not collide", "gotcha",
		"/home/user/projects/foo", "/home/user/projects/foo")

	// Caller passes a DIFFERENT parent directory but the same basename.
	// The naive canonical(basename) comparison would match; the both-abs guard must reject it.
	results, err := s.SearchLearningsBM25Ctx(context.Background(), "same basename different parent", "/home/user/memory/foo", "", "", 10)
	if err != nil {
		t.Fatalf("SearchLearningsBM25Ctx: %v", err)
	}
	for _, r := range results {
		if r.Project == "/home/user/projects/foo" {
			t.Fatalf("expected /home/user/projects/foo NOT to match caller /home/user/memory/foo (same basename, different repo); got result: %+v", r)
		}
	}
}

// TestSearchLearningsBM25Ctx_ProjectFilter_LegacyEmptyCanonicalMatches covers the
// legacy-rows case: rows with empty canonical_project and empty project must remain
// discoverable when a project filter is applied (they are old rows that predate the
// canonical_project column). The empty-empty guard in projectMatchesTolerant handles this.
func TestSearchLearningsBM25Ctx_ProjectFilter_LegacyEmptyCanonicalMatches(t *testing.T) {
	s := mustOpen(t)
	// Insert a learning with no project/canonical_project set (legacy shape)
	id := insertTestLearningWithTime(t, s, "legacy row without project metadata must still be findable", "gotcha", time.Now().UTC())
	_ = id

	results, err := s.SearchLearningsBM25Ctx(context.Background(), "legacy row without project", "/home/user/code/yesmem", "", "", 10)
	if err != nil {
		t.Fatalf("SearchLearningsBM25Ctx: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected legacy row (empty project/canonical_project) to remain discoverable with project filter; got 0")
	}
}

// TestSearchLearningsBM25Ctx_SingleTokenQuery covers the case where the caller passes
// a single token (e.g. a variable name like "min_chars" or "assoc_context"). The tiered
// AND search previously skipped the 1-term tier entirely, returning 0 hits even when
// FTS5 had matching rows. Single-token queries are common when searching for identifiers.
func TestSearchLearningsBM25Ctx_SingleTokenQuery(t *testing.T) {
	s := mustOpen(t)
	insertTestLearningWithTime(t, s, "config field think_reminder_min_chars gates injection by user text length", "decision", time.Now().UTC())

	results, err := s.SearchLearningsBM25Ctx(context.Background(), "min_chars", "", "", "", 10)
	if err != nil {
		t.Fatalf("SearchLearningsBM25Ctx: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected single-token query 'min_chars' to hit FTS5 rows containing the token; got 0")
	}
}

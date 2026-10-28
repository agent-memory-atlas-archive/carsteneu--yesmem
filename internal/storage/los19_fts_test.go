package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-53: schema.go drops the FTS triggers on purpose (background sync instead),
// so no write path may bring one back.
func TestLos19_UpdateLearningContentDoesNotRecreateFTSTrigger(t *testing.T) {
	s := mustOpen(t)
	id, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "old content about flush ordering",
		Confidence: 1.0, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	if err := s.UpdateLearningContent(id, "new content about reindex marker"); err != nil {
		t.Fatalf("UpdateLearningContent: %v", err)
	}

	var triggers int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'learnings_fts_update'`,
	).Scan(&triggers); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if triggers != 0 {
		t.Fatalf("learnings_fts_update trigger exists after UpdateLearningContent; schema.go drops it on purpose")
	}

	var hits int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM learnings_fts WHERE learnings_fts MATCH 'reindex'`,
	).Scan(&hits); err != nil {
		t.Fatalf("fts match: %v", err)
	}
	if hits != 1 {
		t.Fatalf("updated content not reachable in FTS: hits=%d, want 1", hits)
	}
}

// YM-53: InsertLearning rewrites the FTS row to carry anticipated queries. The
// rewrite used the FTS5 'delete' command, which fails on a plain FTS5 table, so
// the following INSERT hit the rowid constraint. Both errors were swallowed and
// anticipated queries never became searchable.
func TestLos19_InsertLearningMakesAnticipatedQueriesSearchable(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.InsertLearning(&models.Learning{
		Category:           "gotcha",
		Content:            "proxy collapse budget shrinks the window",
		Confidence:         1.0,
		CreatedAt:          time.Now(),
		AnticipatedQueries: []string{"zebrafinch quota anomaly"},
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}

	var hits int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM learnings_fts WHERE learnings_fts MATCH 'zebrafinch'`,
	).Scan(&hits); err != nil {
		t.Fatalf("fts match: %v", err)
	}
	if hits != 1 {
		t.Fatalf("anticipated query term not searchable: hits=%d, want 1", hits)
	}

	var contentHits int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM learnings_fts WHERE learnings_fts MATCH 'collapse'`,
	).Scan(&contentHits); err != nil {
		t.Fatalf("fts match content: %v", err)
	}
	if contentHits != 1 {
		t.Fatalf("learning content not searchable after the FTS rewrite: hits=%d, want 1", contentHits)
	}
}

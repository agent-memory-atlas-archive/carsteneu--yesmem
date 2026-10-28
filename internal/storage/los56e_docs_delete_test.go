package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestLos56eDeleteDocSourceClearsTheLearningSideTables is the regression test for
// YM-367 (2): DeleteDocSource hard-deleted the docs_extracted learnings and their
// learnings_fts rows but left the junction tables and the anticipated-query index
// behind. foreign_keys is off (no PRAGMA enables it), so the declared ON DELETE
// CASCADE never fires: every removed document left a permanent orphan in five
// tables, and anticipated_queries_fts kept answering searches with text of a
// learning that no longer exists.
func TestLos56eDeleteDocSourceClearsTheLearningSideTables(t *testing.T) {
	s := mustOpen(t)
	mustUpsertDocSource(t, s, &DocSource{Name: "los56e-docs", Project: ""})
	ds, err := s.GetDocSource("los56e-docs", "")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}
	chunkID, err := s.InsertDocChunk(&DocChunk{
		SourceID:   ds.ID,
		SourceFile: "guide.md",
		SourceHash: "hash-guide",
		Content:    "a documented fact",
	})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	id, err := s.InsertLearning(&models.Learning{
		SessionID:          "los56e-docs-session",
		Category:           "gotcha",
		Content:            "a fact extracted from the guide",
		Confidence:         1.0,
		CreatedAt:          time.Now(),
		ModelUsed:          "test",
		Source:             "docs_extracted",
		DocChunkRef:        chunkID,
		Entities:           []string{"guide"},
		Keywords:           []string{"documented"},
		Actions:            []string{"read"},
		AnticipatedQueries: []string{"what does the guide say"},
	})
	if err != nil {
		t.Fatalf("insert docs learning: %v", err)
	}

	if _, err := s.DeleteDocSource("los56e-docs", ""); err != nil {
		t.Fatalf("delete doc source: %v", err)
	}

	for _, q := range []struct{ name, query string }{
		{"learning", `SELECT COUNT(*) FROM learnings WHERE id = ?`},
		{"entities", `SELECT COUNT(*) FROM learning_entities WHERE learning_id = ?`},
		{"keywords", `SELECT COUNT(*) FROM learning_keywords WHERE learning_id = ?`},
		{"actions", `SELECT COUNT(*) FROM learning_actions WHERE learning_id = ?`},
		{"anticipated queries", `SELECT COUNT(*) FROM learning_anticipated_queries WHERE learning_id = ?`},
		{"anticipated queries fts", `SELECT COUNT(*) FROM anticipated_queries_fts WHERE learning_id = ?`},
		{"learnings fts", `SELECT COUNT(*) FROM learnings_fts WHERE rowid = ?`},
	} {
		var n int
		if err := s.readerDB().QueryRow(q.query, id).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d row(s) for the deleted learning", q.name, n)
		}
	}
}

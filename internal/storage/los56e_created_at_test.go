package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestLos56eInsertLearningNeverWritesAnEmptyCreatedAt is the regression test for
// YM-308 (3a): InsertLearningBatch rendered a zero CreatedAt through fmtTime,
// which returns "". Such a row carries no instant, so every julianday() window
// (since/before in search, get_learnings_since, pulse) drops it: 332 live rows
// were in that state. Insertion time is the honest fallback.
func TestLos56eInsertLearningNeverWritesAnEmptyCreatedAt(t *testing.T) {
	s := mustOpen(t)

	id, err := s.InsertLearning(&models.Learning{
		SessionID:  "los56e-no-time",
		Category:   "gotcha",
		Content:    "the extractor passed no session start",
		Confidence: 1.0,
		ModelUsed:  "test",
		Source:     "llm_extracted",
		// CreatedAt deliberately zero, as extraction does for a session whose
		// started_at never parsed.
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var created string
	if err := s.readerDB().QueryRow(`SELECT COALESCE(created_at, '') FROM learnings WHERE id = ?`, id).Scan(&created); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if created == "" {
		t.Fatal("created_at is empty: the row is invisible to every time window")
	}
	if _, err := time.Parse(time.RFC3339, created); err != nil {
		t.Fatalf("created_at %q is not an instant: %v", created, err)
	}
}

// TestLos56eStoreOpenBackfillsEmptyCreatedAt is the regression test for
// YM-308 (3b): rows written before the default existed keep an empty
// created_at. The session start is the only evidence the store holds, so the
// backfill takes it and leaves a row without a datable session alone.
func TestLos56eStoreOpenBackfillsEmptyCreatedAt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "yesmem.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO sessions (id, project, project_short, started_at, jsonl_path, indexed_at, message_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"los56e-dated", "/home/x/proj", "proj", "2026-10-01T10:00:00+02:00", "/x/a.jsonl", "2026-10-01T10:00:00+02:00", 12); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO sessions (id, project, project_short, started_at, jsonl_path, indexed_at, message_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"los56e-undated", "/home/x/proj", "proj", "", "/x/b.jsonl", "2026-10-01T10:00:00+02:00", 12); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	for _, row := range []struct{ session, content string }{
		{"los56e-dated", "damaged row with a datable session"},
		{"los56e-undated", "damaged row without any date evidence"},
	} {
		if _, err := s.DB().Exec(`INSERT INTO learnings (session_id, category, content, confidence, created_at, model_used, source)
			VALUES (?, 'gotcha', ?, 1.0, '', 'test', 'llm_extracted')`, row.session, row.content); err != nil {
			t.Fatalf("insert damaged learning: %v", err)
		}
	}
	s.Close()

	// Reopening runs the migration list, which is where the backfill lives.
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	var dated, undated string
	if err := s2.readerDB().QueryRow(`SELECT COALESCE(created_at, '') FROM learnings WHERE content LIKE 'damaged row with%'`).Scan(&dated); err != nil {
		t.Fatalf("read dated: %v", err)
	}
	if dated == "" {
		t.Fatal("a row with a datable session must get the session start, it stays invisible otherwise")
	}
	if _, err := time.Parse(time.RFC3339, dated); err != nil {
		t.Fatalf("backfilled created_at %q is not an instant: %v", dated, err)
	}
	if err := s2.readerDB().QueryRow(`SELECT COALESCE(created_at, '') FROM learnings WHERE content LIKE 'damaged row without%'`).Scan(&undated); err != nil {
		t.Fatalf("read undated: %v", err)
	}
	if undated != "" {
		t.Fatalf("a row without date evidence must stay unchanged, got %q", undated)
	}

	// Idempotent on the next open: the stored value must not move.
	s2.Close()
	s3, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen again: %v", err)
	}
	defer s3.Close()
	var again string
	if err := s3.readerDB().QueryRow(`SELECT COALESCE(created_at, '') FROM learnings WHERE content LIKE 'damaged row with%'`).Scan(&again); err != nil {
		t.Fatalf("read dated again: %v", err)
	}
	if again != dated {
		t.Fatalf("backfill is not idempotent: %q became %q", dated, again)
	}
}

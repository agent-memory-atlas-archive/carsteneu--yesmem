package storage

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los10bOffset is ahead of UTC, so a lexical comparison against a UTC bound
// flips the verdict while the parsed instants do not.
var los10bOffset = time.FixedZone("LOS10B", 2*3600)

// insertTimedLos10bLearning inserts a learning whose created_at is written with
// an explicit offset, the way fmtTime writes every real row.
func insertTimedLos10bLearning(t *testing.T, s *Store, content string, createdAt time.Time) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		SessionID:  "sess-los10b-time",
		Category:   "gotcha",
		Content:    content,
		Project:    "los10b-proj",
		Confidence: 1.0,
		CreatedAt:  createdAt,
		ModelUsed:  "test",
		Source:     "llm_extracted",
	})
	if err != nil {
		t.Fatalf("insert learning %q: %v", content, err)
	}
	return id
}

// TestSearchLearningsBM25CtxComparesParsedTimestampsLos10b (YM-246 [MEM-F29]):
// the BM25 post-filter compared created_at strings against since/before. With
// created_at in +02:00 and a UTC bound the row was kept although its instant was
// outside the window.
func TestSearchLearningsBM25CtxComparesParsedTimestampsLos10b(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id := insertTimedLos10bLearning(t, s, "los10b timestamp sentinel", time.Now().Add(-time.Hour).In(los10bOffset))

	// Control: a bound two hours back keeps the row, so the FTS query itself matches.
	hits, err := s.SearchLearningsBM25Ctx(ctx, "los10b timestamp sentinel", "", time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), "", 5)
	if err != nil {
		t.Fatalf("bm25 control: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != strconv.FormatInt(id, 10) {
		t.Fatalf("control failed: expected the one row from a since two hours back, got %d hits", len(hits))
	}

	bound := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	hits, err = s.SearchLearningsBM25Ctx(ctx, "los10b timestamp sentinel", "", bound, "", 5)
	if err != nil {
		t.Fatalf("bm25 since filter: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("learning one hour old was returned with since=%s: %d hits", bound, len(hits))
	}

	// before: the same row is older than the bound and has to survive.
	beforeHits, err := s.SearchLearningsBM25Ctx(ctx, "los10b timestamp sentinel", "", "", bound, 5)
	if err != nil {
		t.Fatalf("bm25 before filter: %v", err)
	}
	if len(beforeHits) != 1 {
		t.Fatalf("learning one hour old was dropped with before=%s: %d hits", bound, len(beforeHits))
	}
}

// TestGetActiveLearningsComparesParsedTimestampsLos10b (YM-246 [MEM-F29]):
// get_learnings passed since/before straight into SQLite, which compares the
// column as text. Offsets in created_at made the window shift by hours.
func TestGetActiveLearningsComparesParsedTimestampsLos10b(t *testing.T) {
	s := newTestStore(t)

	insideID := insertTimedLos10bLearning(t, s, "los10b inside window", time.Now().Add(-5*time.Minute).In(los10bOffset))
	outsideID := insertTimedLos10bLearning(t, s, "los10b outside window", time.Now().Add(-2*time.Hour).In(los10bOffset))

	bound := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	sinceRows, err := s.GetActiveLearnings("gotcha", "los10b-proj", bound, "", 0)
	if err != nil {
		t.Fatalf("active learnings since: %v", err)
	}
	sinceIDs := map[int64]bool{}
	for _, l := range sinceRows {
		sinceIDs[l.ID] = true
	}
	if !sinceIDs[insideID] {
		t.Errorf("learning five minutes old missing with since=%s", bound)
	}
	if sinceIDs[outsideID] {
		t.Errorf("learning two hours old returned with since=%s", bound)
	}

	beforeRows, err := s.GetActiveLearnings("gotcha", "los10b-proj", "", bound, 0)
	if err != nil {
		t.Fatalf("active learnings before: %v", err)
	}
	beforeIDs := map[int64]bool{}
	for _, l := range beforeRows {
		beforeIDs[l.ID] = true
	}
	if beforeIDs[insideID] {
		t.Errorf("learning five minutes old returned with before=%s", bound)
	}
	if !beforeIDs[outsideID] {
		t.Errorf("learning two hours old missing with before=%s", bound)
	}
}

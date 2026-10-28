package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-34: models.Learning.CurrentTurnCount is read by the scoring functions but
// was never filled by any production path, so TurnBasedDecay always returned
// 1.0 and the project boost was pinned at 1.5x. The read path is the single
// place where every consumer (briefing, get_learnings, claude.md) gets its
// learnings, so it resolves the counters there.
func TestLos16ActiveLearningsCarryCurrentTurnCount(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	project := t.TempDir()
	for i := 0; i < 3; i++ {
		if _, err := s.IncrementTurnCount(project); err != nil {
			t.Fatalf("increment turn count: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := s.IncrementTurnCount("__global__"); err != nil {
			t.Fatalf("increment global turn count: %v", err)
		}
	}

	if _, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "los16 project scoped turn note", Project: project,
		Confidence: 1.0, CreatedAt: time.Now(), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert project learning: %v", err)
	}
	if _, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "los16 global turn note",
		Confidence: 1.0, CreatedAt: time.Now(), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert global learning: %v", err)
	}

	got, err := s.GetActiveLearnings("", project, "", "", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d learnings, want 2", len(got))
	}

	for _, l := range got {
		switch l.Project {
		case project:
			if l.CurrentTurnCount != 3 {
				t.Errorf("project learning has CurrentTurnCount=%d, want 3", l.CurrentTurnCount)
			}
		case "":
			if l.CurrentTurnCount != 2 {
				t.Errorf("global learning has CurrentTurnCount=%d, want 2", l.CurrentTurnCount)
			}
		default:
			t.Errorf("unexpected learning project %q", l.Project)
		}
	}
}

// The enrichment must not read through the single write connection: hooks and
// briefings read learnings while extraction and maintenance batches hold a write
// transaction, and a read on that connection waits until the transaction ends.
func TestLos16ActiveLearningsDoNotBlockOnOpenWriteTx(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "yesmem.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	project := t.TempDir()
	if _, err := s.IncrementTurnCount(project); err != nil {
		t.Fatalf("increment turn count: %v", err)
	}
	if _, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "los16 blocking probe", Project: project,
		Confidence: 1.0, CreatedAt: time.Now(), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}

	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin write tx: %v", err)
	}
	defer tx.Rollback()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.GetActiveLearnings("", project, "", "", 0)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("GetActiveLearnings blocked for 5s while a write transaction was open: the turn-count enrichment reads through the single write connection")
	}
}

package briefing

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-106: Generate runs for background refreshes whose output is never
// delivered (regenerateBriefingsForTargets feeds the refine cache). It must not
// burn the 12h deadline cooldown; only the delivery path may mark the hit.
func TestGenerateDoesNotBurnDeadlineCooldown(t *testing.T) {
	store := setupStore(t)
	project := "/var/www/myproject"
	today := time.Now().Format("2006-01-02")

	id, err := store.InsertLearning(&models.Learning{
		Category:    "unfinished",
		Content:     "deadline task",
		Project:     project,
		TaskType:    "task",
		TriggerRule: "deadline:" + today,
		CreatedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}

	gen := New(store, 3)
	text := gen.Generate(project)
	if text == "" {
		t.Fatal("empty briefing")
	}

	hits := gen.DeadlineHits()
	if len(hits) != 1 || hits[0].ID != id || hits[0].Content != "deadline task" {
		t.Fatalf("DeadlineHits() = %v, want one hit for learning %d", hits, id)
	}
	l, err := store.GetLearning(id)
	if err != nil {
		t.Fatalf("GetLearning: %v", err)
	}
	if l.LastHitAt != nil {
		t.Errorf("Generate wrote last_hit_at = %v; cooldown must only start on delivery", l.LastHitAt)
	}

	gen.MarkDeadlineHits(text)
	l, err = store.GetLearning(id)
	if err != nil {
		t.Fatalf("GetLearning after mark: %v", err)
	}
	if l.LastHitAt == nil {
		t.Error("MarkDeadlineHits did not write last_hit_at for a deadline in the delivered text")
	}
}

// The refinement step can replace the generated text with cached prose that no
// longer contains the deadline. Marking that hit would silence a deadline the
// session never read.
func TestMarkDeadlineHitsSkipsItemsMissingFromDeliveredText(t *testing.T) {
	store := setupStore(t)
	project := "/var/www/myproject"
	today := time.Now().Format("2006-01-02")

	id, err := store.InsertLearning(&models.Learning{
		Category:    "unfinished",
		Content:     "deadline task",
		Project:     project,
		TaskType:    "task",
		TriggerRule: "deadline:" + today,
		CreatedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}

	gen := New(store, 3)
	gen.Generate(project)
	gen.MarkDeadlineHits("I'm back. Refined prose without any open work.")

	l, err := store.GetLearning(id)
	if err != nil {
		t.Fatalf("GetLearning: %v", err)
	}
	if l.LastHitAt != nil {
		t.Errorf("deadline marked although the delivered text does not contain it: last_hit_at = %v", l.LastHitAt)
	}
}

// A triggered deadline is urgent by definition and must survive the reminder
// mode importance filter.
func TestDeadlineSurvivesImportanceFilterInReminderMode(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	project := "/var/www/quiet"
	today := time.Now().Format("2006-01-02")
	// Most recent session 6h ago -> reminder mode (absence >= 4h).
	store.UpsertSession(&models.Session{
		ID: "old", Project: project, ProjectShort: "quiet",
		StartedAt: time.Now().Add(-6 * time.Hour), MessageCount: 5,
		JSONLPath: "/old.jsonl", IndexedAt: time.Now(),
	})
	if _, err := store.InsertLearning(&models.Learning{
		Category:    "unfinished",
		Content:     "low importance deadline",
		Project:     project,
		TaskType:    "task",
		Importance:  1,
		TriggerRule: "deadline:" + today,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}

	gen := New(store, 3)
	text := gen.Generate(project)
	if !strings.Contains(text, "low importance deadline") {
		t.Fatalf("triggered deadline was filtered out in reminder mode:\n%s", text)
	}
	if hits := gen.DeadlineHits(); len(hits) != 1 {
		t.Errorf("expected one rendered deadline hit, got %v", hits)
	}
}

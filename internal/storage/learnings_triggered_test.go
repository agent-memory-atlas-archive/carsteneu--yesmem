package storage

import (
	"fmt"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-106: GetTriggeredLearnings limited to 10 rows without ordering or rule
// filter, so ten unrelated trigger rules could push every deadline out of the
// result. The deadline prefix filter must make the deadline rows reachable.
func TestGetTriggeredLearnings_DeadlineRowsNotCrowdedOut(t *testing.T) {
	store := mustOpen(t)
	now := time.Now()
	project := "proj"

	// The active-id index scans newest first, so the deadline rows are the
	// oldest here: twelve newer trigger rules then fill the LIMIT 10 window and
	// hide both deadlines until the deadline prefix filter applies.
	tomorrow := time.Now().Add(24 * time.Hour).Format("2006-01-02")
	for i := 0; i < 2; i++ {
		if _, err := store.InsertLearning(&models.Learning{
			Category:    "unfinished",
			Content:     fmt.Sprintf("deadline task %d", i),
			Project:     project,
			TaskType:    "task",
			TriggerRule: "deadline:" + tomorrow,
			CreatedAt:   now,
		}); err != nil {
			t.Fatalf("insert deadline %d: %v", i, err)
		}
	}
	for i := 0; i < 12; i++ {
		if _, err := store.InsertLearning(&models.Learning{
			Category:    "gotcha",
			Content:     fmt.Sprintf("non-deadline trigger %d", i),
			Project:     project,
			TriggerRule: "review:check",
			CreatedAt:   now,
		}); err != nil {
			t.Fatalf("insert non-deadline %d: %v", i, err)
		}
	}

	got, err := store.GetTriggeredLearnings(project, "deadline:", "")
	if err != nil {
		t.Fatalf("GetTriggeredLearnings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected the 2 deadline rows, got %d", len(got))
	}
	for _, l := range got {
		if l.TriggerRule != "deadline:"+tomorrow {
			t.Errorf("non-deadline row leaked into the deadline lookup: %q", l.TriggerRule)
		}
	}
}

// Rows with an unrelated trigger rule stay invisible to a deadline lookup.
func TestGetTriggeredLearnings_OnlyMatchingPrefix(t *testing.T) {
	store := mustOpen(t)

	if _, err := store.InsertLearning(&models.Learning{
		Category:    "gotcha",
		Content:     "review reminder",
		Project:     "proj",
		TriggerRule: "review:check",
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := store.GetTriggeredLearnings("proj", "deadline:", "")
	if err != nil {
		t.Fatalf("GetTriggeredLearnings: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no deadline rows, got %d", len(got))
	}
}

// YM-106: long-expired deadline rows sort first under ORDER BY trigger_rule, so
// without a lower bound they consume the LIMIT 10 window and hide the upcoming
// deadline. The minRule bound keeps the window on relevant dates.
func TestGetTriggeredLearnings_ExpiredDeadlinesDoNotHideUpcoming(t *testing.T) {
	store := mustOpen(t)
	project := "proj"
	now := time.Now()

	for i := 0; i < 10; i++ {
		old := now.AddDate(0, 0, -(60 + i)).Format("2006-01-02")
		if _, err := store.InsertLearning(&models.Learning{
			Category:    "unfinished",
			Content:     fmt.Sprintf("expired deadline %d", i),
			Project:     project,
			TaskType:    "task",
			TriggerRule: "deadline:" + old,
			CreatedAt:   now,
		}); err != nil {
			t.Fatalf("insert expired %d: %v", i, err)
		}
	}
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")
	if _, err := store.InsertLearning(&models.Learning{
		Category:    "unfinished",
		Content:     "upcoming deadline",
		Project:     project,
		TaskType:    "task",
		TriggerRule: "deadline:" + tomorrow,
		CreatedAt:   now,
	}); err != nil {
		t.Fatalf("insert upcoming: %v", err)
	}

	minRule := "deadline:" + now.AddDate(0, 0, -1).Format("2006-01-02")
	got, err := store.GetTriggeredLearnings(project, "deadline:", minRule)
	if err != nil {
		t.Fatalf("GetTriggeredLearnings: %v", err)
	}
	if len(got) != 1 || got[0].TriggerRule != "deadline:"+tomorrow {
		t.Fatalf("upcoming deadline hidden by expired rows: got %d rows (%v)", len(got), got)
	}
}

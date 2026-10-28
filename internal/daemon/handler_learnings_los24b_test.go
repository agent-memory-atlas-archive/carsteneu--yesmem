package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

func los24bInsert(t *testing.T, s *storage.Store, category, taskType, content string, age time.Duration) {
	t.Helper()
	if _, err := s.InsertLearning(&models.Learning{
		Content:   content,
		Category:  category,
		TaskType:  taskType,
		Source:    "user_stated",
		CreatedAt: time.Now().Add(-age),
	}); err != nil {
		t.Fatalf("insert %s: %v", content, err)
	}
}

// YM-249: get_learnings filtered task_type in Go, after GetActiveLearnings had
// already capped the row set at limit*5. With more newer unfinished rows of
// another task_type, every wanted row fell out of that window and the answer was
// empty. The filter belongs in the query.
func TestGetLearningsTaskTypeSurvivesNewerOtherTypes(t *testing.T) {
	h, s := mustFileHandler(t)

	// 200 newer ideas and stale entries crowd the oversampling window.
	for i := 0; i < 200; i++ {
		los24bInsert(t, s, "unfinished", "idea", "cap idea "+time.Duration(i).String()+string(rune('a'+i%26)), time.Duration(i)*time.Minute)
	}
	los24bInsert(t, s, "unfinished", "task", "the only real task", 48*time.Hour)

	resp := h.handleGetLearnings(map[string]any{
		"category":  "unfinished",
		"task_type": "task",
		"limit":     float64(30),
	})
	if resp.Error != "" {
		t.Fatalf("get_learnings: %s", resp.Error)
	}
	var got []models.Learning
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Content != "the only real task" {
		contents := make([]string, 0, len(got))
		for _, l := range got {
			contents = append(contents, l.Content)
		}
		t.Fatalf("task_type=task returned %d rows %v, want exactly the one task row", len(got), contents)
	}
	if got[0].TaskType != "task" {
		t.Errorf("task_type = %q, want task", got[0].TaskType)
	}
}

// The same query without a task_type filter must keep returning every newer row:
// the pushdown must not narrow the unfiltered path.
func TestGetLearningsWithoutTaskTypeKeepsAllTypes(t *testing.T) {
	h, s := mustFileHandler(t)
	los24bInsert(t, s, "unfinished", "idea", "an idea", time.Minute)
	los24bInsert(t, s, "unfinished", "task", "a task", 2*time.Minute)

	resp := h.handleGetLearnings(map[string]any{"category": "unfinished", "limit": float64(30)})
	if resp.Error != "" {
		t.Fatalf("get_learnings: %s", resp.Error)
	}
	var got []models.Learning
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("unfiltered get_learnings returned %d rows, want 2", len(got))
	}
}

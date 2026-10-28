package briefing

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-107: ideas are stored as category="unfinished" with task_type="idea", so
// the old hint get_learnings(category="unfinished:idea") always returned 0 hits.
func TestOpenWorkIdeaHintUsesTaskType(t *testing.T) {
	s := DefaultStrings()
	g := &Generator{strings: s}
	items := []models.Learning{
		{ID: 1, Content: "Real task", TaskType: "task", Importance: 4},
		{ID: 2, Content: "Someday idea", TaskType: "idea", Importance: 4},
	}
	result := g.renderOpenWork(s, items, 2.0, nil)

	if strings.Contains(result, `category="unfinished:idea"`) {
		t.Errorf("idea hint must not use category=\"unfinished:idea\" (yields 0 hits):\n%s", result)
	}
	// category is required too: the handler filters task_type after the row
	// limit, and unfinished rows sort last in the unscoped query.
	if !strings.Contains(result, `get_learnings(category="unfinished", task_type="idea")`) {
		t.Errorf("idea hint must pre-filter by category and task_type:\n%s", result)
	}
}

// YM-109: the pivot overflow hint rendered "get_learnings get_learnings(...)".
func TestKnowledgePivotHintNotDuplicated(t *testing.T) {
	s := DefaultStrings()
	data := KnowledgeData{
		Pivots:     []string{"[ID:1] a pivot"},
		MorePivots: 2,
	}
	result := renderTemplate("knowledge", tmplKnowledge, s, data)

	if strings.Contains(result, "get_learnings get_learnings") {
		t.Errorf("pivot hint duplicates the tool name:\n%s", result)
	}
	if got := strings.Count(result, `get_learnings("pivot_moment")`); got != 1 {
		t.Errorf("expected exactly one pivot_moment hint, got %d:\n%s", got, result)
	}
}

// YM-109: urgency reasons already read "Deadline ...", the template added
// another "Deadline: " prefix -> "Deadline: Deadline morgen".
func TestOpenWorkDeadlineReasonNotDuplicated(t *testing.T) {
	s := DefaultStrings()
	g := &Generator{strings: s}
	items := []models.Learning{
		{ID: 42, Content: "Billing Migration", TriggerRule: "deadline:2026-03-28", Importance: 4},
	}
	reasons := map[int64]string{42: formatDeadlineReason(s, 1.2)}
	result := g.renderOpenWork(s, items, 0, reasons)

	if strings.Contains(result, "Deadline: Deadline") {
		t.Errorf("deadline prefix is duplicated:\n%s", result)
	}
	if !strings.Contains(result, "(Deadline: tomorrow)") {
		t.Errorf("expected a single, readable deadline suffix, got:\n%s", result)
	}
}

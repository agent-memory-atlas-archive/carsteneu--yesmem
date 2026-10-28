package briefing

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-261 (Los 11d): since YM-251 resolve_by_text refuses any text with more than
// ten words or without a unique full match, so the cap-idea dismiss hint taught
// a call that cannot work for a real cap-idea sentence. The briefing already
// prints the learning ID next to every cap idea, so it must point at
// resolve(learning_id=...) instead.
func TestRenderOpenWork_Los11d_CapIdeaDismissUsesTheLearningID(t *testing.T) {
	s := DefaultStrings()
	g := &Generator{strings: s}
	items := []models.Learning{
		{ID: 4242, Content: "Cap: Telegram polling; yesmem telegram poll", TaskType: "cap_idea", MatchCount: 4, Importance: 1},
	}

	result := g.renderOpenWork(s, items, 5.0, nil)

	if !strings.Contains(result, "resolve(learning_id=<ID>)") {
		t.Fatalf("expected an id-based dismiss hint, got %q", result)
	}
	if !strings.Contains(result, "[#4242]") {
		t.Fatalf("the idea must stay listed with its [#ID] so the hint has a target, got %q", result)
	}
	if strings.Contains(result, "resolve_by_text") {
		t.Fatalf("the dismiss hint must not teach resolve_by_text any more, got %q", result)
	}
}

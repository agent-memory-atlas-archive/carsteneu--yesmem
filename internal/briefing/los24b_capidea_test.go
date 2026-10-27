package briefing

import (
	"regexp"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

var los24bArgRe = regexp.MustCompile(`(category|task_type)="([^"]*)"`)

func los24bCapIdeaArgs(t *testing.T, out string) map[string]string {
	t.Helper()
	args := map[string]string{}
	for _, m := range los24bArgRe.FindAllStringSubmatch(out, -1) {
		args[m[1]] = m[2]
	}
	return args
}

// YM-249: the cap-idea confirm hint told the reader to call
// remember(category="unfinished:task"). That category does not exist;
// handleRemember silently degrades an unknown category to explicit_teaching, so
// the confirmed idea landed outside the unfinished list. A hint must name a
// category the write path accepts.
func TestCapIdeaConfirmHintNamesAnAcceptedCategory(t *testing.T) {
	got := capIdeasText(DefaultStrings(), []models.Learning{{ID: 7, Content: "deploy flow", MatchCount: 2}})
	args := los24bCapIdeaArgs(t, got)

	cat := args["category"]
	if cat == "" {
		t.Fatalf("cap-idea confirm hint has no category argument: %q", got)
	}
	if !models.IsValidCategory(cat) {
		t.Errorf("cap-idea hint recommends category %q, which IsValidCategory rejects", cat)
	}
	if cat != "unfinished" {
		t.Errorf("cap-idea hint category = %q, want unfinished: the confirmed idea is an open task", cat)
	}
	// The kind of unfinished entry is not expressible as a category, it is a
	// separate argument.
	if args["task_type"] != "task" {
		t.Errorf("cap-idea hint task_type = %q, want task (categories carry no task_type suffix)", args["task_type"])
	}
}

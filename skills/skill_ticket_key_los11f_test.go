package skills

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// YM-279 (Los 11f): a commit closes an open task only when its subject names the
// task by ticket key or learning id (internal/storage/learnings_commit.go). At
// the audit 0 of 1358 open tasks carried a key, so the anchor could never fire.
// New tasks get the key from three channels, and each one is pinned here so a
// rewrite of the instruction text cannot silently drop the convention.
const (
	yesRememberSkillPath = "bundled-skills/yesmem-remember/SKILL.md"
	extractionPromptPath = "../internal/extraction/prompt.go"
)

func readBundled(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// The brief template is what an orchestrator pastes into the scratchpad, and the
// loop task is derived from it. Without the key in the Task line, the task the
// loop closes has no key either. Anchored on the template itself: the skill has
// other "Task type:" fields, and a match anywhere would pass on a decoy.
func TestBriefTemplateCarriesTheTicketKey_Los11f(t *testing.T) {
	skill := readBundled(t, yesloopSkillPath)

	template := strings.Index(skill, `section="yesloop-<task-slug>"`)
	if template < 0 {
		t.Fatalf("%s: the brief template moved", yesloopSkillPath)
	}
	rest := skill[template:]
	if end := strings.Index(rest, "`)"); end > 0 {
		rest = rest[:end]
	}
	line := regexp.MustCompile(`Task:[^\n]*`).FindString(rest)
	if line == "" {
		t.Fatalf("the brief template lost its Task line")
	}
	if !regexp.MustCompile(`YM-\d`).MatchString(line) {
		t.Fatalf("the brief Task line carries no ticket key placeholder: %q", line)
	}
}

// remember() is the manual channel: a task saved without a key cannot be closed
// by a commit, which is exactly the gap the audit measured.
func TestRememberSkillCarriesTheTicketKey_Los11f(t *testing.T) {
	skill := readBundled(t, yesRememberSkillPath)

	if !strings.Contains(skill, "task_type") {
		t.Fatalf("%s never mentions task_type, so the open-task convention has no anchor", yesRememberSkillPath)
	}
	if !regexp.MustCompile(`(?i)ticket key|YM-\d`).MatchString(skill) {
		t.Fatalf("%s does not tell the writer to keep the ticket key in an open task", yesRememberSkillPath)
	}
}

// Extraction produces the bulk of open tasks (docs/features/memory.md). Without
// an instruction to preserve the key, the largest channel stays keyless.
func TestExtractionPromptCarriesTheTicketKey_Los11f(t *testing.T) {
	src := readBundled(t, extractionPromptPath)

	start := strings.Index(src, "unfinished: Open tasks")
	if start < 0 {
		t.Fatalf("%s: the unfinished instruction moved", extractionPromptPath)
	}
	end := strings.Index(src[start:], "8. relationships")
	if end < 0 {
		t.Fatalf("%s: the unfinished block lost its end marker", extractionPromptPath)
	}
	block := src[start : start+end]
	if !regexp.MustCompile(`(?i)ticket key|learning id`).MatchString(block) {
		t.Fatalf("the unfinished instruction does not ask for the ticket key or learning id")
	}
}

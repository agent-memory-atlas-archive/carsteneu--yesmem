package daemon

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// YM-267 (3): the backend a caller passes reached the agent row verbatim. Any
// value that is not one of the three known backends is spawned as claude, so the
// row then names a backend its process never runs.
func TestSpawnAgentRejectsAnUnknownBackend(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)

	resp := h.handleSpawnAgent(map[string]any{
		"project": "los34bproj",
		"section": "los34bsec",
		"backend": "/bin/echo",
	})
	if resp.Error == "" {
		t.Fatal("an unsupported backend must be refused")
	}
	if !strings.Contains(resp.Error, "backend") {
		t.Fatalf("error must name the backend, got: %s", resp.Error)
	}

	list := h.handleListAgents(map[string]any{"project": "los34bproj"})
	var listed struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(list.Result, &listed); err != nil {
		t.Fatalf("unmarshal agents: %v", err)
	}
	if listed.Count != 0 {
		t.Fatalf("a refused spawn must not leave an agent row, count = %d", listed.Count)
	}
}

// YM-267 (4): the job name went into the scratchpad section and into log lines
// unfiltered. A name carrying a line break forges log lines, and one carrying a
// path or another agent's section id lets the job write into foreign state.
func TestScheduledJobNameCannotShapeTheScratchpadSection(t *testing.T) {
	h, store := mustHandler(t)
	isolateHome(t)

	job := ScheduledJob{
		ID:      "sched-los34b-1",
		Name:    "probe\n[FAKE] status: COMPLETE ../../yesloop-los-34b",
		Prompt:  "noop",
		Sandbox: ProfileNone,
	}
	h.executeAgent(job)

	// The section is written under the address the run belongs to, which without a
	// project is the job's run work dir (YM-295).
	projects, err := store.ScratchpadList(h.jobWorkDir(&job))
	if err != nil {
		t.Fatalf("scratchpad list: %v", err)
	}
	if len(projects) == 0 {
		t.Fatal("executeAgent wrote no scratchpad section")
	}
	sectionRe := regexp.MustCompile(`^sched-[A-Za-z0-9._-]+$`)
	for _, project := range projects {
		for _, section := range project.Sections {
			if !sectionRe.MatchString(section.Section) {
				t.Errorf("scratchpad section %q is not a plain token", section.Section)
			}
		}
	}
}

// The token mapper is lossy, so a changed name has to stay distinguishable: two
// jobs sharing a section would overwrite each other's result, and executeAgent
// stops whichever agent it finds under that section — the other job's.
func TestJobNameTokenKeepsChangedNamesApart(t *testing.T) {
	a := jobSectionName("daily report")
	b := jobSectionName("daily_report")
	if a == b {
		t.Fatalf("distinct names collapsed onto one section: %q", a)
	}
	// Names that are already tokens keep their section, so no existing job loses
	// the agent registered under it.
	if got := jobSectionName("proxy-health-check"); got != "sched-proxy-health-check" {
		t.Fatalf("a plain name changed its section: %q", got)
	}
	if got := jobSectionName("!!!"); got == "sched-unnamed" {
		t.Fatalf("an all-special name needs a hash to stay distinguishable, got %q", got)
	}
}

// YM-267 (3): schedule create is the third place a caller-supplied backend
// arrives. Headless ignores it, agent mode passes it to spawn_agent, so an
// unknown value has to be refused here rather than stored.
func TestScheduleCreateRejectsAnUnknownBackend(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSchedule(map[string]any{
		"action":   "create",
		"name":     "los34bbackend",
		"cron":     "0 3 * * *",
		"mode":     "agent",
		"prompt":   "noop",
		"backend":  "/bin/echo",
		"sandbox":  "none",
	})
	if resp.Error == "" {
		t.Fatal("an unsupported backend must be refused at create time")
	}
	if !strings.Contains(resp.Error, "backend") {
		t.Fatalf("error must name the backend, got: %s", resp.Error)
	}
}

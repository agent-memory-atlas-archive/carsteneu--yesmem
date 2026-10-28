package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-417 [OPS-101]: the scheduler and the bash error handler picked a cap row by
// name alone. After YM-403 the catalog, activate_cap and loadCapMeta resolve one
// row per name (own project first, then global, a foreign project never), but
// both execution paths kept walking GetActiveLearnings and took the first row
// that carried the name. A project that shadows a name — or a foreign project
// that happens to sort first — therefore ran or was repaired instead of the
// caller's row.

// seedLos58dShadowCap writes one active cap row with a single bash script named
// "run". created_at is caller-chosen because the resolution walked a list
// ordered by created_at DESC: the test has to know which row comes first.
func seedLos58dShadowCap(t *testing.T, h *Handler, name, project, body string, at time.Time) {
	t.Helper()
	meta := CapMeta{
		Name:    name,
		Scripts: []ScriptMeta{{Name: "run", Kind: "handler", Runtime: "bash", Body: body}},
		Version: 1,
		Tested:  true,
	}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("cap meta json: %v", err)
	}
	if _, err := h.store.InsertLearning(&models.Learning{
		Category:  "cap",
		Content:   name,
		Project:   project,
		Context:   ctx,
		Source:    "user_stated",
		CreatedAt: at,
	}); err != nil {
		t.Fatalf("insert cap: %v", err)
	}
}

// los58dShadowPair seeds the caller's own row first and a NEWER row of the same
// name second, so the newest row heads the list the old resolution walked.
func los58dShadowPair(t *testing.T, h *Handler, name, project, ownBody, shadowProject, shadowBody string) {
	t.Helper()
	seedLos58dShadowCap(t, h, name, project, ownBody, time.Now().Add(-2*time.Minute))
	seedLos58dShadowCap(t, h, name, shadowProject, shadowBody, time.Now().Add(-time.Minute))
}

func TestLos58dSchedulerRunsTheCallersOwnCapRow(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	los58dShadowPair(t, h, "los58d_shadow", project, "echo OWN", "", "echo GLOBAL")

	resolved, err := h.los35cResolveBashCap(&ScheduledJob{
		ID: "los58d-own", CapName: "los58d_shadow", ScriptName: "run", Project: project,
	})
	if err != nil {
		t.Fatalf("resolve bash cap: %v", err)
	}
	if resolved.body != "echo OWN" {
		t.Errorf("the scheduler took the shadowing row: body = %q, want the caller's own %q", resolved.body, "echo OWN")
	}
	if !resolved.projectBound {
		t.Error("the caller's own row carries a project, so the run is project-bound")
	}
}

func TestLos58dSchedulerNeverRunsAnotherProjectsCapRow(t *testing.T) {
	h, _ := mustHandler(t)
	project, other := t.TempDir(), t.TempDir()
	los58dShadowPair(t, h, "los58d_foreign", project, "echo OWN", other, "echo FOREIGN")

	resolved, err := h.los35cResolveBashCap(&ScheduledJob{
		ID: "los58d-foreign", CapName: "los58d_foreign", ScriptName: "run", Project: project,
	})
	if err != nil {
		t.Fatalf("resolve bash cap: %v", err)
	}
	if resolved.body != "echo OWN" {
		t.Errorf("the scheduler took another project's body: body = %q, want %q", resolved.body, "echo OWN")
	}
}

// The fire path is the second half of the same question: a job that cannot reach
// the row has to stop before running anything.
func TestLos58dSchedulerRefusesACapOfAnotherProject(t *testing.T) {
	h, _ := mustHandler(t)
	other := t.TempDir()
	seedLos58dShadowCap(t, h, "los58d_bound_elsewhere", other, "echo FOREIGN", time.Now())

	resolved, err := h.los35cResolveBashCap(&ScheduledJob{
		ID: "los58d-unreachable", CapName: "los58d_bound_elsewhere", ScriptName: "run", Project: t.TempDir(),
	})
	if err == nil {
		t.Fatalf("a job outside the cap's project resolved its body: %q", resolved.body)
	}
}

// The bash error handler asks a model to repair a script and saves the result
// into the cap. If it reads the shadowing row, it repairs foreign code and
// writes the model's output into the wrong row. The prompt is the observable
// point where the chosen row becomes visible.
func TestLos58dBashErrorHandlerRepairsTheCallersOwnCapRow(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)
	project := t.TempDir()

	ownBody := "#!/bin/bash\nset -eu\necho own-marker\nexit 0\n"
	ownFixed := "#!/bin/bash\nset -eu\necho own-fixed\nexit 0\n"
	if got := ClassifyCapDiff(ownBody, ownFixed); got != DiffSubstantial {
		t.Fatalf("test setup: this rewrite classifies as DiffSubstantial, got %v", got)
	}
	los58dShadowPair(t, h, "los58d_repair", project, ownBody, "", "#!/bin/bash\nset -eu\necho global-marker\nexit 0\n")

	client := &autoCorrectStubClient{reply: patchReply("echo own-marker", "echo own-fixed")}
	h.SetAutoCorrectClient(client)
	h.autoCorrectVerify = autoCorrectPassVerifier(t)

	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	if err := h.scheduler.AddJob(ScheduledJob{
		ID:          "los58d-repair",
		Name:        "los58d repair",
		Cron:        "*/5 * * * *",
		Enabled:     true,
		Mode:        "bash",
		CapName:     "los58d_repair",
		ScriptName:  "run",
		AutoCorrect: true,
		Project:     project,
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:      "los58d-repair",
		JobName:    "los58d repair",
		CapName:    "los58d_repair",
		ScriptName: "run",
		Command:    ownBody,
		Status:     "error",
		ExitCode:   1,
		ErrorMsg:   "own-marker: command not found",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	if client.callCount() == 0 {
		t.Fatal("the error handler never asked the model: the fixture no longer reaches the repair path")
	}
	prompt := client.lastPrompt()
	if !strings.Contains(prompt, "own-marker") {
		t.Errorf("the repair prompt does not carry the caller's script:\n%s", prompt)
	}
	if strings.Contains(prompt, "global-marker") {
		t.Error("the repair prompt carries the shadowing row's body: the handler picked the wrong row")
	}
}

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-346/OPS-85, key 2: the save lock keeps a project-bound cap off the
// unsandboxed path, but the scheduler resolves the profile on its own
// (script, else job, else default) and schedule create validated no project
// binding at all. A job with sandbox=none therefore ran a project-bound cap on
// the host without save_cap ever being touched.
//
// The lock belongs on the level where the run actually happens: the effective
// profile, which is what fireJobBash computes. Both entry points are pinned
// here — create refuses early, and a legacy row that already exists is refused
// at fire time.
func los35cSeedCap(t *testing.T, h *Handler, name, project, scope, scriptSandbox, body string) {
	t.Helper()
	meta := CapMeta{
		Name: name,
		Scripts: []ScriptMeta{{
			Name:    "run",
			Kind:    "handler",
			Runtime: "bash",
			Body:    body,
			Sandbox: scriptSandbox,
		}},
		Version: 1,
		Tested:  true,
		Scope:   scope,
	}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("cap meta json: %v", err)
	}
	if _, err := h.store.InsertLearning(&models.Learning{
		Category: "cap",
		Content:  name,
		Project:  project,
		Context:  ctx,
		Source:   "user_stated",
	}); err != nil {
		t.Fatalf("insert cap: %v", err)
	}
}

func TestLos35cScheduleCreateRefusesUnsandboxedProjectBoundCap(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	los35cSeedCap(t, h, "los35c_bound", "/home/u/proj", "project", "", "touch /nonexistent-should-not-run")

	resp := h.handleSchedule(map[string]any{
		"action":      "create",
		"name":        "los35c_unsandboxed",
		"cron":        "*/5 * * * *",
		"mode":        "bash",
		"cap_name":    "los35c_bound",
		"script_name": "run",
		"sandbox":     "none",
		"project":     "/home/u/proj",
	})
	if resp.Error == "" {
		t.Fatal("schedule create accepted sandbox=none for a project-bound cap — that is the bypass the save lock closes")
	}
	for _, want := range []string{"los35c_bound", "project-bound"} {
		if !strings.Contains(resp.Error, want) {
			t.Errorf("error should name %q, got %s", want, resp.Error)
		}
	}
	if jobs := h.scheduler.ListJobs(); len(jobs) != 0 {
		t.Errorf("a refused create must not leave a job behind, got %d", len(jobs))
	}
}

// The binding is what the lock is keyed on: a user-scoped cap may run
// unsandboxed, exactly as save_cap allows it.
func TestLos35cScheduleCreateAllowsUserScopedUnsandboxedJob(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	los35cSeedCap(t, h, "los35c_user", "", "user", "", "echo ok")

	resp := h.handleSchedule(map[string]any{
		"action":      "create",
		"name":        "los35c_user_job",
		"cron":        "*/5 * * * *",
		"mode":        "bash",
		"cap_name":    "los35c_user",
		"script_name": "run",
		"sandbox":     "none",
	})
	if resp.Error != "" {
		t.Fatalf("a user-scoped cap may run unsandboxed, got %s", resp.Error)
	}
}

// The script profile wins over the job profile at run time, so a project-bound
// cap whose script names one is not unsandboxed even under a sandbox=none job.
// Refusing that would be a false positive.
func TestLos35cScheduleCreateAllowsProjectBoundCapWithOwnProfile(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	los35cSeedCap(t, h, "los35c_profiled", "/home/u/proj", "project", "standard", "echo ok")

	resp := h.handleSchedule(map[string]any{
		"action":      "create",
		"name":        "los35c_profiled_job",
		"cron":        "*/5 * * * *",
		"mode":        "bash",
		"cap_name":    "los35c_profiled",
		"script_name": "run",
		"sandbox":     "none",
		"project":     "/home/u/proj",
	})
	if resp.Error != "" {
		t.Fatalf("a named script profile keeps the run sandboxed, got %s", resp.Error)
	}
}

// A cap that this call cannot resolve to a bash script is no evidence of an
// unsandboxed run: fireJobBash stops on the same resolution error before it
// executes anything. Refusing at create would report a lock violation where the
// real problem is a job that cannot run at all.
func TestLos35cScheduleCreateLeavesUnresolvableCapToTheFirePath(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	meta := CapMeta{
		Name: "los35c_repl",
		Scripts: []ScriptMeta{{
			Name:    "run",
			Kind:    "tool",
			Runtime: "repl",
			Body:    "async () => 1",
			Schema:  "{}",
		}},
		Version: 1,
		Scope:   "project",
	}
	ctx, err := meta.ToJSON()
	if err != nil {
		t.Fatalf("cap meta json: %v", err)
	}
	if _, err := h.store.InsertLearning(&models.Learning{
		Category: "cap",
		Content:  "los35c_repl",
		Project:  "/home/u/proj",
		Context:  ctx,
		Source:   "user_stated",
	}); err != nil {
		t.Fatalf("insert cap: %v", err)
	}

	resp := h.handleSchedule(map[string]any{
		"action":      "create",
		"name":        "los35c_repl_job",
		"cron":        "*/5 * * * *",
		"mode":        "bash",
		"cap_name":    "los35c_repl",
		"script_name": "run",
		"sandbox":     "none",
		"project":     "/home/u/proj",
	})
	if resp.Error != "" {
		t.Fatalf("a job that resolves to no bash script must fail at fire, not as a lock violation: %s", resp.Error)
	}
}

// Only a bash job runs a cap script. An agent or headless job may carry a
// cap_name it never executes, and refusing it would be a false positive.
func TestLos35cScheduleCreateIgnoresTheCapForNonBashModes(t *testing.T) {
	h, _ := mustHandler(t)
	h.scheduler = newScheduler(nil, nil)
	los35cSeedCap(t, h, "los35c_bound", "/home/u/proj", "project", "", "echo hi")

	resp := h.handleSchedule(map[string]any{
		"action":   "create",
		"name":     "los35c_agent_job",
		"cron":     "*/5 * * * *",
		"mode":     "agent",
		"prompt":   "say hello",
		"cap_name": "los35c_bound",
		"sandbox":  "none",
	})
	if resp.Error != "" {
		t.Fatalf("an agent job does not run the cap, so the sandbox lock does not apply: %s", resp.Error)
	}
}

// A row that predates the create check keeps its profile in the database, so the
// fire path has to refuse as well. The proof is the marker the script would
// create: it must not exist, and the run has to land as an error record.
func TestLos35cFireJobBashRefusesUnsandboxedProjectBoundCap(t *testing.T) {
	h, _ := mustHandler(t)
	marker := filepath.Join(t.TempDir(), "ran")
	los35cSeedCap(t, h, "los35c_bound", "/home/u/proj", "project", "", "touch "+marker)

	job := &ScheduledJob{
		ID:         "los35c-legacy-row",
		Name:       "los35c legacy",
		Mode:       "bash",
		CapName:    "los35c_bound",
		ScriptName: "run",
		Project:    "/home/u/proj",
		Sandbox:    ProfileNone,
	}
	h.fireJobBash(job)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the job ran the script unsandboxed — the fire path must refuse a project-bound cap")
	}

	runs, err := h.store.GetBashJobRuns(job.ID, 1)
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if len(runs) == 0 {
		t.Fatal("a refused run must still be recorded, otherwise the job looks green")
	}
	if runs[0].Status != "error" {
		t.Errorf("run status = %q, want error", runs[0].Status)
	}
	if !strings.Contains(runs[0].ErrorMsg, "project-bound") {
		t.Errorf("run error should name the reason, got %q", runs[0].ErrorMsg)
	}
}

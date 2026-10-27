package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los32gProject is the absolute project directory a job is created from.
func los32gProject(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	return dir
}

// los32gScratchpad returns the content of one section, or "" when it is absent.
func los32gScratchpad(t *testing.T, s *storage.Store, project, section string) string {
	t.Helper()
	sections, err := s.ScratchpadRead(project, section)
	if err != nil {
		t.Fatalf("read scratchpad %s/%s: %v", project, section, err)
	}
	for _, sc := range sections {
		if sc.Section == section {
			return sc.Content
		}
	}
	return ""
}

// YM-295 (1): every scheduler-driven write and agent registration addressed the
// literal "yesmem", the daemon's own checkout, whatever project the job belonged
// to. The row and the task section therefore have to sit under the job's project,
// or stop/relay/get cannot find the agent and a run reports into a foreign one.
func TestLos32gExecuteAgentAddressesTheJobsProject(t *testing.T) {
	neutralizeClaude(t)
	isolateHome(t)
	h, s := los32dHandler(t)
	h.disableAgentProcesses = true
	proj := los32gProject(t)

	job := ScheduledJob{ID: "sched-los32g-agent", Name: "los32g-agent", Project: proj, Backend: "claude", Prompt: "report"}
	h.executeAgent(job)

	section := jobSectionName(job.Name)
	agent, err := s.AgentGetBySection(proj, section)
	if err != nil || agent == nil {
		t.Fatalf("no agent registered under the job's project %q: %v", proj, err)
	}
	if agent.Project != proj {
		t.Errorf("agent project = %q, want the job's project %q", agent.Project, proj)
	}
	if body := los32gScratchpad(t, s, proj, section); !strings.Contains(body, "SCHEDULED TASK") {
		t.Errorf("task section under %q = %q, want the scheduled task", proj, body)
	}
	if body := los32gScratchpad(t, s, "yesmem", section); body != "" {
		t.Errorf("the task section was written under the daemon's own checkout name: %q", body)
	}
	// The run's own bookkeeping addresses the agent by (project, section).
	if resp := h.handleGetAgent(map[string]any{"to": section, "project": proj}); resp.Error != "" {
		t.Errorf("get_agent under the job's project: %s", resp.Error)
	}
}

// YM-295 (4): a headless run used to write an allow-all .claude/settings.local.json
// into the job's real repository, and a symlinked .claude left it without any
// allowlist until the timeout. The permission bypass belongs on the command line;
// the project stays untouched.
func TestLos32gHeadlessRunWritesNoPermissionsIntoTheProject(t *testing.T) {
	isolateHome(t)
	h, s := los32dHandler(t)
	proj := los32gProject(t)

	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '{\"type\":\"result\",\"result\":\"headless ok\",\"session_id\":\"sess-los32g\"}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	job := ScheduledJob{
		ID: "sched-los32g-headless", Name: "los32g-headless", Prompt: "report",
		Project: proj, Mode: "headless", Sandbox: ProfileNone,
	}
	h.executeHeadless(job)

	if _, err := os.Stat(filepath.Join(proj, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Errorf("the headless run wrote an allow-all permissions file into the job's project: %v", err)
	}
	if body := los32gScratchpad(t, s, proj, jobSectionName(job.Name)); !strings.Contains(body, "headless ok") {
		t.Errorf("result section under the job's project = %q, want the run result", body)
	}
	if body := los32gScratchpad(t, s, "yesmem", jobSectionName(job.Name)); body != "" {
		t.Errorf("the result was written under the daemon's own checkout name: %q", body)
	}
}

// YM-295 (4): without a permission bypass on the command line a headless run
// waits for a prompt nobody answers, which is the timeout the ticket describes.
func TestLos32gHeadlessArgsCarryThePermissionBypass(t *testing.T) {
	args := strings.Join(headlessArgs("", ""), " ")
	if !strings.Contains(args, "--permission-mode bypassPermissions") {
		t.Fatalf("headless args = %q, want --permission-mode bypassPermissions", args)
	}
}

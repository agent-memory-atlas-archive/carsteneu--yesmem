package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/orchestrator"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los57eCaptureSpawnArgs stubs the bridge seam and returns the command line the
// spawn passed to the backend. The bridge refuses to start, so no process and no
// terminal window comes into existence and the captured args are all the test
// needs.
func los57eCaptureSpawnArgs(t *testing.T) <-chan []string {
	t.Helper()
	prev := newAgentBridge
	args := make(chan []string, 1)
	newAgentBridge = func(name string, got []string, sock, workDir string, extraEnv ...string) (*orchestrator.AgentBridge, error) {
		select {
		case args <- append([]string(nil), got...):
		default:
		}
		return nil, errors.New("no bridge in this test")
	}
	t.Cleanup(func() { newAgentBridge = prev })
	return args
}

// los57eClaudeAllowlist is the grant the daemon used to write into the project as
// a .claude/settings.local.json. It has to reach the backend as a command line
// argument instead, because the file outlives the run and widens the user's own
// interactive sessions in that repository (YM-295, YM-300).
func los57eClaudeAllowlist(t *testing.T, args []string) string {
	t.Helper()
	for i, arg := range args {
		if arg == "--allowedTools" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// A claude agent spawn must leave the repository untouched: the allow-all
// permission file lands in a real checkout, survives the run, and grants
// Bash(*)/Write(*) to every later interactive session of the user in that repo.
func TestLos57eSpawnAgentLeavesNoPermissionFileInTheRepository(t *testing.T) {
	h, _ := mustHandler(t)
	h.disableAgentProcesses = false
	repo := t.TempDir()
	captured := los57eCaptureSpawnArgs(t)

	resp := h.handleSpawnAgent(map[string]any{
		"project":  repo,
		"section":  "los57e-spawn",
		"work_dir": repo,
		"backend":  "claude",
	})
	if resp.Error != "" {
		t.Fatalf("spawn_agent: %s", resp.Error)
	}

	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Fatalf("the spawn wrote an allow-all permission file into the project %s", repo)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude")); !os.IsNotExist(err) {
		t.Errorf("the spawn created %s/.claude in the project", repo)
	}

	select {
	case args := <-captured:
		if got := los57eClaudeAllowlist(t, args); !strings.Contains(got, "Bash(*)") {
			t.Errorf("claude args carry no allowlist (%q): with the file gone the run would prompt and hang", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stubbed bridge was never reached")
	}
}

// A resume passed only --resume, so it relied on the permission file for its
// allowlist. With the file gone the resumed agent would prompt for every tool.
func TestLos57eClaudeResumeCarriesTheAllowlist(t *testing.T) {
	h, s := mustHandler(t)
	h.disableAgentProcesses = false
	repo := t.TempDir()
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-los57e-resume", Project: repo, Section: "los57e-resume",
		SessionID: "sess-los57e", Status: "finished", Backend: "claude", WorkDir: repo,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	captured := los57eCaptureSpawnArgs(t)

	resp := h.handleResumeAgent(map[string]any{"to": "agent-los57e-resume"})
	if resp.Error != "" {
		t.Fatalf("resume_agent: %s", resp.Error)
	}

	select {
	case args := <-captured:
		if !strings.Contains(strings.Join(args, " "), "--resume") {
			t.Fatalf("captured args are not a resume: %v", args)
		}
		if got := los57eClaudeAllowlist(t, args); !strings.Contains(got, "Bash(*)") {
			t.Fatalf("resume args carry no allowlist (%v): the resumed agent would prompt for every tool", args)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stubbed bridge was never reached")
	}
}

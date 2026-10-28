package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/carsteneu/yesmem/internal/orchestrator"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los58eSpawnLabels drives one spawn of the given backend and reports the argv the
// backend received and the title the terminal was opened with.
func los58eSpawnLabels(t *testing.T, h *Handler, id, project, section string) (args []string, title string) {
	t.Helper()
	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("true not available: %v", err)
	}
	var mu sync.Mutex
	prevBridge, prevDisplay := newAgentBridge, agentDisplayCommand
	newAgentBridge = func(name string, gotArgs []string, sock, workDir string, extraEnv ...string) (*orchestrator.AgentBridge, error) {
		mu.Lock()
		args = append([]string{}, gotArgs...)
		mu.Unlock()
		return orchestrator.NewAgentBridge(trueBin, nil, sock, workDir)
	}
	agentDisplayCommand = func(terminal, binPath, gotTitle string, innerArgs ...string) (string, []string) {
		mu.Lock()
		title = gotTitle
		mu.Unlock()
		return trueBin, nil
	}
	t.Cleanup(func() { newAgentBridge, agentDisplayCommand = prevBridge, prevDisplay })

	sockPath := filepath.Join(shortSockDir(t), id+".sock")
	if err := h.store.AgentCreate(storage.Agent{
		ID: id, Project: project, Section: section, SessionID: "sess-" + id,
		Status: "pending", Backend: "claude", SockPath: sockPath,
	}); err != nil {
		t.Fatalf("AgentCreate(%s): %v", id, err)
	}
	// A fresh claude spawn carries the --name this test is about. The bind is
	// refused (the stub child does not name the backend), so the prompt-inject
	// goroutine never starts.
	h.spawnAgentProcess(id, "sess-"+id, project, section, "", sockPath, t.TempDir(),
		"claude", "", "", 0, 0, "", false)
	return args, title
}

// hasTerminalControl reports whether a value carries a character a terminal
// would interpret: C0 (ESC starts every ANSI sequence, BEL is in the title
// terminator) plus DEL and C1.
func los58eHasTerminalControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}

// TestLos58e_SpawnLabelsAreSanitized covers YM-416: section and project end up in
// the claude --name value and in the terminal window title, both rendered by a
// terminal. handleSpawnAgent refuses control characters in those fields since
// YM-211, but open_agent_terminal takes the project as a parameter without that
// gate, so the values have to be safe where they are consumed.
func TestLos58e_SpawnLabelsAreSanitized(t *testing.T) {
	h, _ := mustHandler(t)
	const project = "/repo/\x1b[2J\x1b]0;forged\x07proj"
	const section = "sec-\x1b[31mred\x07-\u202e"

	args, title := los58eSpawnLabels(t, h, "los58e-label", project, section)

	name := ""
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			name = args[i+1]
		}
		if los58eHasTerminalControl(a) {
			t.Errorf("argv carries terminal control: %q", a)
		}
	}
	if name == "" {
		t.Fatalf("no --name argument in %q", args)
	}
	if los58eHasTerminalControl(title) {
		t.Errorf("window title carries terminal control: %q", title)
	}
	// The cap itself is covered by TestLos58e_SpawnLabelCapBites; the fixture here
	// is deliberately short, so a length assertion would be vacuous.
	if !strings.Contains(name, "repo") || !strings.Contains(name, "red") {
		t.Errorf("--name lost the readable part of the labels: %q", name)
	}
}

// TestLos58e_SpawnLabelCapBites: the 64-rune cap must actually cut. Without this
// case the length assertion in the sanitization test is satisfied by a fixture
// far below the limit and would stay green if the cap were removed (cold review
// finding).
func TestLos58e_SpawnLabelCapBites(t *testing.T) {
	h, _ := mustHandler(t)
	long := strings.Repeat("s", 200)
	args, title := los58eSpawnLabels(t, h, "los58e-cap", "/p", long)

	if want := "/p-" + strings.Repeat("s", 64); spawnNameArg(t, args) != want {
		t.Errorf("--name = %q, want the project plus exactly 64 runes of the section", spawnNameArg(t, args))
	}
	if want := "yesmem-" + strings.Repeat("s", 64) + " #los58e-cap"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
}

// spawnNameArg returns the value of the --name argument.
func spawnNameArg(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("no --name argument in %q", args)
	return ""
}

// TestLos58e_SpawnLabelsLeavePlainNamesAlone keeps the sanitization from
// rewriting the normal case: the title and --name of an ordinary spawn are
// unchanged.
func TestLos58e_SpawnLabelsLeavePlainNamesAlone(t *testing.T) {
	h, _ := mustHandler(t)
	args, title := los58eSpawnLabels(t, h, "los58e-plain", "/home/u/proj", "yesloop-los-58e")

	if want := "yesmem-yesloop-los-58e #los58e-plain"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	want := "/home/u/proj-yesloop-los-58e"
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			if args[i+1] != want {
				t.Errorf("--name = %q, want %q", args[i+1], want)
			}
			return
		}
	}
	t.Fatalf("no --name argument in %q", args)
}

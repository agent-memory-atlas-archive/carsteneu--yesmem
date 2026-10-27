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

// YM-287 [OPS-65]: the socket cleanup in the supervisor paths still followed
// agents.sock_path, which update_agent and register_agent write verbatim. Every
// one of them removes the id-derived path instead (YM-274).
// The helpers are the los38e ones on purpose: same package, no second copy, and
// no new package-level name that a parallel loop could pick.

func TestSockPathOwnership_CrashRecoveryNeverUnlinksTheColumn(t *testing.T) {
	h, s := mustHandler(t)
	h.disableAgentProcesses = true
	foreign := los38eForeignSockFile(t)
	own := los38eOwnSockFile(t, h)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj",
		Status: "running", Backend: "claude",
		// A PID nobody holds: crashRecovery has to treat the row as gone.
		PID:      99999999,
		SockPath: foreign,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	// Retries exhausted, so no replacement is spawned in the background.
	if err := s.AgentUpdate("agent-1", map[string]any{"retry_count": 3}); err != nil {
		t.Fatalf("set retry_count: %v", err)
	}

	h.crashRecovery()

	los38eAssertExists(t, foreign, foreign+".inject")
	los38eAssertGone(t, own, own+".inject")
}

func TestSockPathOwnership_AttemptRestartUsesDerivedSocketPath(t *testing.T) {
	h, s := mustHandler(t)
	h.disableAgentProcesses = false
	foreign := los38eForeignSockFile(t)
	workDir := t.TempDir()

	bridged := make(chan string, 1)
	prevBridge := newAgentBridge
	newAgentBridge = func(name string, args []string, sock, dir string, extraEnv ...string) (*orchestrator.AgentBridge, error) {
		bridged <- sock
		return nil, errors.New("stop before a real backend starts")
	}
	t.Cleanup(func() { newAgentBridge = prevBridge })

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", WorkDir: workDir,
		Status: "error", Backend: "claude",
		RestartStrategy: "permanent", MaxRestarts: 3,
		SockPath:  foreign,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	h.attemptRestart()

	select {
	case sock := <-bridged:
		if want := filepath.Join(h.dataDir, "agent-1.sock"); sock != want {
			t.Errorf("restart bound the bridge to %q, want the id-derived %q — the column is caller-writable", sock, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("attemptRestart never reached the bridge, so the spawn path was not exercised")
	}

	los38eAssertExists(t, foreign, foreign+".inject")
}

func TestSockPathOwnership_KillAgentNeverUnlinksTheColumn(t *testing.T) {
	h, s := mustHandler(t)
	foreign := los38eForeignSockFile(t)
	own := los38eOwnSockFile(t, h)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj",
		Status: "running", Backend: "claude",
		PID: 99999999, SockPath: foreign,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	agent, err := s.AgentGet("agent-1")
	if err != nil || agent == nil {
		t.Fatalf("AgentGet: %v", err)
	}

	h.killAgent(agent)

	los38eAssertExists(t, foreign, foreign+".inject")
	los38eAssertGone(t, own, own+".inject")
}

// The derivation guard is the only thing that keeps a caller-chosen id from
// carrying the unlink out of the data directory, now that every supervisor path
// relies on it. A guard without a test is a guard nobody has seen work.
func TestSockPathOwnership_DerivationGuardsRejectForeignIDs(t *testing.T) {
	h, _ := mustHandler(t)
	for _, tc := range []struct {
		name string
		id   string
		want string
	}{
		{"plain id", "agent-1", filepath.Join(h.dataDir, "agent-1.sock")},
		{"separator", "link/victim", ""},
		{"parent", "../victim", ""},
		{"absolute", "/etc/passwd", ""},
		{"daemon socket", "daemon", ""},
		{"job socket", "job", ""},
		{"empty id", "", ""},
		{"dot becomes a plain name", ".", filepath.Join(h.dataDir, "..sock")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.agentSockPath(tc.id); got != tc.want {
				t.Errorf("agentSockPath(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}

	if got := (&Handler{}).agentSockPath("agent-1"); got != "" {
		t.Errorf("agentSockPath with an empty dataDir = %q, want \"\"", got)
	}
}

// The cleanup must refuse the same ids: the path the guard rejects would
// otherwise walk through a symlinked directory inside the data directory.
func TestSockPathOwnership_RefusedIDDeletesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		file string
	}{
		{"separator through symlink", "link/victim", "victim.sock"},
		{"reserved daemon socket", "daemon", "daemon.sock"},
		{"reserved job socket", "job", "job.sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := mustHandler(t)
			outside := t.TempDir()
			victim := filepath.Join(outside, tc.file)
			if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
				t.Fatalf("write probe: %v", err)
			}
			if tc.id == "daemon" || tc.id == "job" {
				// The reserved sockets live in the data directory itself.
				victim = filepath.Join(h.dataDir, tc.file)
				if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
					t.Fatalf("write probe: %v", err)
				}
			} else if err := os.Symlink(outside, filepath.Join(h.dataDir, "link")); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			if err := s.AgentCreate(storage.Agent{
				ID: tc.id, Project: "proj", Section: "sec-refused",
				Status: "running", Backend: "claude",
				// Bound on purpose: an empty column skips the cleanup, which is how
				// the older guard tests reached nothing.
				SockPath: victim,
			}); err != nil {
				t.Fatalf("AgentCreate: %v", err)
			}

			if resp := h.handleStopAgent(map[string]any{"to": tc.id}); resp.Error != "" {
				t.Fatalf("stop_agent: %s", resp.Error)
			}

			los38eAssertExists(t, victim)
		})
	}
}

// net.Listen("unix", "") binds an abstract socket instead of failing, so a
// spawn without a derivable path would come up unreachable. It has to be
// refused and the row closed.
func TestSockPathOwnership_SpawnRefusesUnderivedPath(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "daemon", Project: "proj", Section: "sec-refused-spawn",
		Status: "spawning", Backend: "claude",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	sockPath, resp := h.agentSockPathForSpawn("daemon")
	if sockPath != "" {
		t.Errorf("agentSockPathForSpawn returned %q, want \"\" for an id that names a daemon socket", sockPath)
	}
	if resp.Error == "" {
		t.Fatal("agentSockPathForSpawn accepted an underivable id — the spawn would bind an abstract socket")
	}

	a, err := s.AgentGet("daemon")
	if err != nil || a == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if a.Status != "error" || a.Error == "" {
		t.Errorf("row status=%q error=%q, want error with a reason", a.Status, a.Error)
	}
}

// The helper test above does not prove the call sites propagate the refusal: a
// handler that drops the response compiles and spawns on an abstract socket. An
// empty dataDir is the only way to reach the refusal without an unusable id, and
// it keeps the test away from every live service.
func TestSockPathOwnership_CallSitesPropagateTheRefusal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive func(h *Handler, workDir string) Response
	}{
		{
			name: "spawn_agent",
			drive: func(h *Handler, workDir string) Response {
				return h.handleSpawnAgent(map[string]any{
					"project": "proj", "section": "sec-call-site", "backend": "claude",
					"work_dir": workDir,
				})
			},
		},
		{
			name: "open_agent_terminal",
			drive: func(h *Handler, workDir string) Response {
				return h.handleOpenAgentTerminal(map[string]any{
					"project": "proj", "backend": "claude", "work_dir": workDir,
				})
			},
		},
		{
			name: "resume_agent",
			drive: func(h *Handler, workDir string) Response {
				return h.handleResumeAgent(map[string]any{"to": "agent-1", "project": "proj"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := mustHandler(t)
			workDir := t.TempDir()
			if err := s.AgentCreate(storage.Agent{
				ID: "agent-1", Project: "proj", Section: "sec-call-site",
				Status: "stopped", Backend: "claude",
				SessionID: "sess-call-site", WorkDir: workDir,
			}); err != nil {
				t.Fatalf("AgentCreate: %v", err)
			}
			h.dataDir = ""

			resp := tc.drive(h, workDir)
			if resp.Error == "" {
				t.Fatal("handler accepted a spawn without a derivable socket path")
			}
			if !strings.Contains(resp.Error, "socket path could not be derived") {
				t.Fatalf("handler refused for another reason: %q — the refusal path was not reached", resp.Error)
			}
		})
	}
}

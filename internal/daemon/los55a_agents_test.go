package daemon

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// startSigtermIgnoringAgent starts a process that ignores SIGTERM and survives it.
// It is the audit's case for YM-244 key 3: a TUI that does not act on the request
// keeps its pid, its session and its section forever, and no guard looks at a
// closed row.
func startSigtermIgnoringAgent(t *testing.T) (<-chan struct{}, int) {
	t.Helper()
	// `exec` keeps the pid: the process that survives has to be the one the row
	// names, not a child of a shell that would die on the signal.
	cmd := exec.Command("sh", "-c", `trap "" TERM; exec sleep 30`)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sigterm-ignoring process: %v", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	return exited, pid
}

// YM-244 [AGT-MA33] key 3: after the SIGTERM the stop only waited. A process that
// ignores SIGTERM kept running behind a row that read "stopped", and nothing
// escalated — the daemon's own stop escalates to SIGKILL (lifecycle.go), the
// agent stop did not.
func TestHandleStopAgent_EscalatesToSigkillLos55a(t *testing.T) {
	requireProcTable(t)
	h, s := mustHandler(t)
	exited, pid := startSigtermIgnoringAgent(t)
	stubProcCmdline(t, "/usr/local/bin/claude\x00--session-id\x00x")

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-sigkill", Project: "proj", Section: "ignores-term",
		Status: "running", Backend: "claude", PID: pid,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	if resp := h.handleStopAgent(map[string]any{"to": "agent-sigkill"}); resp.Error != "" {
		t.Fatalf("stop: %s", resp.Error)
	}

	// Reaping is the supervisor's job: a killed child stays visible as a zombie
	// until it is waited for, and a liveness probe cannot tell the two apart.
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the process survived the stop: SIGTERM was ignored and nothing escalated to SIGKILL")
	}
}

// YM-244 [AGT-MA33] key 1: a respawn dropped the spawn-time max_turns. The
// replacement came back on the daemon's config default (30), so an agent spawned
// with a tight or a huge turn budget changed its own limit by crashing — the one
// path nobody watches.
func TestRespawnAgent_InheritsMaxTurnsLos55a(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "respawn-maxturns", Project: "respawn-maxturns-proj", Section: "respawn-maxturns-sec",
		Status: "crashed", Backend: "opencode", MaxTurns: 7,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	crashed, err := s.AgentGet("respawn-maxturns")
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}

	h.respawnAgent(*crashed)

	agents, err := s.AgentList("respawn-maxturns-proj")
	if err != nil {
		t.Fatalf("AgentList: %v", err)
	}
	var replacement *storage.Agent
	for i := range agents {
		if agents[i].ID != "respawn-maxturns" {
			replacement = &agents[i]
		}
	}
	if replacement == nil {
		t.Fatal("respawnAgent created no replacement agent")
	}
	if replacement.MaxTurns != 7 {
		t.Errorf("replacement max_turns = %d, want 7 — the spawn-time turn budget was lost in the respawn",
			replacement.MaxTurns)
	}
}

// A spawn that names max_turns has to persist it: the row is the only place the
// value survives a crash, and the heartbeat's relay cap reads it from there.
func TestHandleSpawnAgent_PersistsMaxTurnsLos55a(t *testing.T) {
	h, s := mustHandler(t)
	h.agentDefaultBackend = "claude"

	resp := h.handleSpawnAgent(map[string]any{
		"project":   "proj",
		"work_dir":  t.TempDir(),
		"section":   "max-turns-spawn",
		"max_turns": float64(11),
	})
	if resp.Error != "" {
		t.Fatalf("spawn: %s", resp.Error)
	}
	m := resultMap(t, resp)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("spawn returned no id: %v", m)
	}

	a, err := s.AgentGet(id)
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if a.MaxTurns != 11 {
		t.Errorf("spawned row max_turns = %d, want 11", a.MaxTurns)
	}
}

// YM-244 cold review: the cascade clears the child's pid, so the child's exit tail
// no longer recognizes the row as its own and keeps the socket files. The cascade
// owns the cleanup for that reason: a dead socket at a per-id path accumulates
// otherwise.
func TestStopAgent_CascadeRemovesTheChildrenSocketsLos55a(t *testing.T) {
	h, s := mustHandler(t)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-20261020-90", Project: "proj", Section: "root",
		SessionID: "root-session", Status: "running",
	}); err != nil {
		t.Fatalf("AgentCreate parent: %v", err)
	}
	childSock := h.agentSockPath("agent-20261020-91")
	if childSock == "" {
		t.Fatal("no derivable socket path for the child")
	}
	for _, p := range []string{childSock, childSock + ".inject"} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("fixture %s: %v", p, err)
		}
	}
	// A child with a *live*, verified process: the cascade signals it and the old
	// code then trusted the exit tail with the socket files — which cannot work any
	// more, because the cascade has already cleared the row's pid (so the tail no
	// longer sees the row as its own). The signals are stubbed so the process stays
	// and the files are the only observable.
	_, childPID := startSigtermIgnoringAgent(t)
	stubProcCmdline(t, "/usr/local/bin/claude\x00--session-id\x00x")
	prevSignal := agentProcSignal
	agentProcSignal = func(int, syscall.Signal) error { return nil }
	t.Cleanup(func() { agentProcSignal = prevSignal })

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-20261020-91", Project: "proj", Section: "child",
		SessionID: "child-session", CallerSession: "root-session",
		Status: "running", PID: childPID, SockPath: childSock,
	}); err != nil {
		t.Fatalf("AgentCreate child: %v", err)
	}

	if resp := h.handleStopAgent(map[string]any{"to": "agent-20261020-90"}); resp.Error != "" {
		t.Fatalf("stop: %s", resp.Error)
	}

	for _, p := range []string{childSock, childSock + ".inject"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived the cascade — the child's exit tail keeps it after the pid reset", p)
		}
	}
}

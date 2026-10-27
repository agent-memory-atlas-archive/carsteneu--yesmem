package daemon

import (
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// fakeInjectSocket accepts connections on the inject socket path and records
// every write so tests can assert the exact kick payload (two connections,
// each carrying "\r").
type fakeInjectSocket struct {
	mu          sync.Mutex
	connections int
	writes      []string
}

func (f *fakeInjectSocket) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connections
}

func (f *fakeInjectSocket) allWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func startFakeInjectSocket(t *testing.T, path string) *fakeInjectSocket {
	t.Helper()
	rec := &fakeInjectSocket{}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Count before reading: connect() completes against the kernel
			// backlog before the accept goroutine runs, so a synchronous
			// caller must poll on connections anyway — but counting first
			// keeps the counter the earliest signal.
			rec.mu.Lock()
			rec.connections++
			rec.mu.Unlock()
			buf := make([]byte, 64)
			conn.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := conn.Read(buf)
			rec.mu.Lock()
			rec.writes = append(rec.writes, string(buf[:n]))
			rec.mu.Unlock()
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return rec
}

// waitForConnections polls until the socket saw want connections. Unix
// connect() completes against the kernel backlog before Accept runs, so
// checking the counter directly after checkPermissionKick() is racy.
func waitForConnections(t *testing.T, rec *fakeInjectSocket, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for rec.count() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := rec.count(); got != want {
		t.Fatalf("expected %d connections, got %d", want, got)
	}
}

// awaitNoConnection gives a synchronous kicker a short window to show up,
// then asserts nothing dialed.
func awaitNoConnection(t *testing.T, rec *fakeInjectSocket) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Fatalf("expected no dial, got %d connections", got)
	}
}

// waitForWrites polls until the socket recorded at least want writes.
func waitForWrites(t *testing.T, rec *fakeInjectSocket, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.allWrites()) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return rec.allWrites()
}

func newPermKickHandler(t *testing.T, kickDefault bool) (*Handler, *storage.Store) {
	t.Helper()
	h, s := mustHandler(t)
	h.agentPermissionKick = kickDefault
	permKickPause = 2 * time.Millisecond
	t.Cleanup(func() { permKickPause = 2 * time.Second })
	resetPermissionKickState()
	t.Cleanup(resetPermissionKickState)
	return h, s
}

// agentProcCmdlineStub is the /proc cmdline the tests attribute to the test's
// own PID. C2 gates the kick on the PID being a *verified* agent process, and
// the test binary cannot present an opencode cmdline of its own.
const agentProcCmdlineStub = "/home/tester/.opencode/bin/opencode\x00--theme\x00yesmem-loop"

func createKickAgent(t *testing.T, s *storage.Store, id, sockPath, permissionKick string) storage.Agent {
	t.Helper()
	stubProcCmdline(t, agentProcCmdlineStub)
	agent := storage.Agent{
		ID:             id,
		Project:        "/tmp/proj-" + id,
		Section:        "sec-" + id,
		SessionID:      "sess-" + id,
		PID:            os.Getpid(), // lebt
		SockPath:       sockPath,
		Status:         "running",
		Backend:        "opencode",
		PermissionKick: permissionKick,
	}
	if err := s.AgentCreate(agent); err != nil {
		t.Fatalf("AgentCreate(%s): %v", id, err)
	}
	return agent
}

func seedKickStateNeverKicked(t *testing.T, agentID string, streamIdleFor time.Duration) {
	t.Helper()
	permKickAgentsMu.Lock()
	permKickAgents[agentID] = &permKickState{
		lastStreamActiveAt: time.Now().Add(-streamIdleFor),
	}
	permKickAgentsMu.Unlock()
}

func seedKickState(t *testing.T, agentID string, streamIdleFor, lastKickAgo time.Duration) {
	t.Helper()
	permKickAgentsMu.Lock()
	permKickAgents[agentID] = &permKickState{
		lastStreamActiveAt: time.Now().Add(-streamIdleFor),
		lastKickAt:         time.Now().Add(-lastKickAgo),
	}
	permKickAgentsMu.Unlock()
}

// seedActiveStream registers an active StreamState for the agent's session,
// mirroring the test pattern in yesloop_stagnation_test.go.
func seedActiveStream(sessionID string) {
	streamStates[sessionID] = &StreamState{Active: true, StartedAt: time.Now()}
	sessionToThread[sessionID] = sessionID
}

// shortSockDir keeps the unix socket path under UNIX_PATH_MAX (104): macOS
// t.TempDir() returns /var/folders/... paths that make net.Listen fail with
// "bind: invalid argument" (see handler_agents_test.go relay tests).
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pk")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// 1. Kein Kick wenn stream aktiv.
func TestPermissionKick_StreamActiveNoKick(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-active")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-active", sockPath, "")
	seedActiveStream(agent.SessionID)
	seedKickStateNeverKicked(t, agent.ID, 10*time.Minute)

	h.checkPermissionKick()

	awaitNoConnection(t, rec)
	permKickAgentsMu.Lock()
	refreshed := permKickAgents[agent.ID].lastStreamActiveAt
	permKickAgentsMu.Unlock()
	if time.Since(refreshed) > time.Minute {
		t.Errorf("lastStreamActiveAt not refreshed while stream active: %v", refreshed)
	}
}

// 2. Kein Kick vor 3 Min Idle.
func TestPermissionKick_NoKickBeforeDelay(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-early")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-early", sockPath, "")
	seedKickStateNeverKicked(t, agent.ID, 1*time.Minute)

	h.checkPermissionKick()

	awaitNoConnection(t, rec)
}

// 3. Kick bei >=3 Min Idle, Payload "\r" auf Conn1 + "\r" auf Conn2.
func TestPermissionKick_KickAfterDelay(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-kick")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-kick", sockPath, "")
	seedKickStateNeverKicked(t, agent.ID, 4*time.Minute)

	h.checkPermissionKick()

	waitForConnections(t, rec, 2)
	for i, w := range waitForWrites(t, rec, 2) {
		if w != "\r" {
			t.Errorf("write[%d] = %q, want \\r", i, w)
		}
	}
	permKickAgentsMu.Lock()
	kickAt := permKickAgents[agent.ID].lastKickAt
	permKickAgentsMu.Unlock()
	if kickAt.IsZero() {
		t.Error("lastKickAt not set after kick")
	}
}

// 4. Kein Re-Kick vor 5 Min (Flut-Schutz, Lektion #89414).
func TestPermissionKick_NoReKickBeforeInterval(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-flood")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-flood", sockPath, "")
	seedKickState(t, agent.ID, 10*time.Minute, 1*time.Minute)

	h.checkPermissionKick()

	awaitNoConnection(t, rec)
}

// 5. Re-Kick >=5 Min.
func TestPermissionKick_ReKickAfterInterval(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-rekick")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-rekick", sockPath, "")
	seedKickState(t, agent.ID, 10*time.Minute, 6*time.Minute)

	h.checkPermissionKick()

	waitForConnections(t, rec, 2)
}

// 6. permission_kick="off" blockt.
func TestPermissionKick_AgentOffBlocks(t *testing.T) {
	h, s := newPermKickHandler(t, true) // Config-Default aktiv
	sockPath := los24dAgentSock(t, h, "agent-pk-off")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-off", sockPath, "off")
	seedKickStateNeverKicked(t, agent.ID, 10*time.Minute)

	h.checkPermissionKick()

	awaitNoConnection(t, rec)
}

// 7. Vererbung: Spalte "" + Config true -> Kick; Config false -> kein Kick.
func TestPermissionKick_Inheritance(t *testing.T) {
	h1, s1 := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h1, "agent-pk-inh-on")
	recOn := startFakeInjectSocket(t, sockPath+".inject")
	agent1 := createKickAgent(t, s1, "agent-pk-inh-on", sockPath, "")
	seedKickStateNeverKicked(t, agent1.ID, 10*time.Minute)
	h1.checkPermissionKick()

	h2, s2 := newPermKickHandler(t, false)
	sockPath2 := los24dAgentSock(t, h2, "agent-pk-inh-off")
	recOff := startFakeInjectSocket(t, sockPath2+".inject")
	agent2 := createKickAgent(t, s2, "agent-pk-inh-off", sockPath2, "")
	seedKickStateNeverKicked(t, agent2.ID, 10*time.Minute)
	h2.checkPermissionKick()

	waitForConnections(t, recOn, 2)
	awaitNoConnection(t, recOff)
}

// 8d. Auf einer Plattform ohne lesbare Prozesstabelle wird der Kick nicht
// abgeschaltet: sein Gate ist "der Prozess ist nicht definitiv weg", und die
// einzige Aktion ist ein Enter in den agenteneigenen Socket. Ein recycelter PID
// kann das nicht auf einen Fremden umleiten.
func TestPermissionKick_WithoutProcessTableStillKicksLiveAgent(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-noproc")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-noproc", sockPath, "")
	seedKickStateNeverKicked(t, agent.ID, 10*time.Minute)
	prev := procTableReadable
	procTableReadable = false
	t.Cleanup(func() { procTableReadable = prev })

	h.checkPermissionKick()

	waitForConnections(t, rec, 2)
}

// 8. Gates: ein Agent wird nur gekickt, wenn sein Status kickbar ist UND sein
// PID als eigener Backend-Prozess verifiziert ist (C2). "stopped" mit lebendem
// PID ist damit außerhalb des Scopes, ein toter PID weiter blockierend. Die
// Spalte sock_path ist seit YM-338 kein Gate mehr: der Pfad kommt aus der ID.
func TestPermissionKick_Gates(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-gate-stopped")
	rec := startFakeInjectSocket(t, sockPath+".inject")

	// stopped mit lebendem PID: seit C2 nicht mehr gekickt — die Markierung
	// zählt hier wieder, weil der Prozess weiterlebt, aber der Agent beendet ist.
	stopped := createKickAgent(t, s, "agent-pk-gate-stopped", sockPath, "")
	if err := s.AgentUpdate(stopped.ID, map[string]any{"status": "stopped"}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	seedKickStateNeverKicked(t, stopped.ID, 10*time.Minute)

	// toter PID
	dead := createKickAgent(t, s, "agent-pk-gate-deadpid", "", "")
	if err := s.AgentUpdate(dead.ID, map[string]any{"pid": -1}); err != nil {
		t.Fatalf("update pid: %v", err)
	}
	seedKickStateNeverKicked(t, dead.ID, 10*time.Minute)

	h.checkPermissionKick()

	awaitNoConnection(t, rec)
}

// 8b. Status "pending" gehört zum Kick-Scope (C2): der Prozess lebt, der Agent
// ist noch nicht als running markiert.
func TestPermissionKick_PendingAgentGetsKicked(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-pending")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-pending", sockPath, "")
	if err := s.AgentUpdate(agent.ID, map[string]any{"status": "pending"}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	seedKickStateNeverKicked(t, agent.ID, 10*time.Minute)

	h.checkPermissionKick()

	waitForConnections(t, rec, 2)
}

// 8c. Ein Fremdprozess auf dem PID des Agenten wird nie gekickt. signal 0 sagt
// "lebt" (der Bystander läuft), die cmdline sagt "nicht opencode" — genau die
// Unterscheidung, die isPIDAlive allein nicht treffen kann (C2, C1).
func TestPermissionKick_BystanderProcessNotKicked(t *testing.T) {
	requireProcTable(t)
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-bystander")
	rec := startFakeInjectSocket(t, sockPath+".inject")

	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatalf("start bystander: %v", err)
	}
	t.Cleanup(func() { bystander.Process.Kill(); bystander.Wait() })

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-pk-bystander", Project: "/tmp/proj-bystander", Section: "sec-bystander",
		SessionID: "sess-bystander", PID: bystander.Process.Pid, SockPath: sockPath,
		Status: "running", Backend: "opencode",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	seedKickStateNeverKicked(t, "agent-pk-bystander", 10*time.Minute)

	h.checkPermissionKick()

	awaitNoConnection(t, rec)
}

// 9. Paused Agent mit lebendem PID wird gekickt — Status-Markierung ist egal.
func TestPermissionKick_PausedAgentGetsKicked(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-paused")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-paused", sockPath, "")
	if err := s.AgentUpdate(agent.ID, map[string]any{"status": "paused"}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	seedKickStateNeverKicked(t, agent.ID, 4*time.Minute)

	h.checkPermissionKick()

	waitForConnections(t, rec, 2)
}

// 10. Auto-Unpause: pausierter Agent mit lebendem PID und wieder aktivem
// Stream wird zurück auf running geflippt — Beweis von Leben statt Markierung.
func TestPermissionKick_PausedAutoUnpauseOnRecovery(t *testing.T) {
	h, s := newPermKickHandler(t, true)
	sockPath := los24dAgentSock(t, h, "agent-pk-unpause")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	agent := createKickAgent(t, s, "agent-pk-unpause", sockPath, "")
	if err := s.AgentUpdate(agent.ID, map[string]any{"status": "paused"}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	seedActiveStream(agent.SessionID)
	seedKickStateNeverKicked(t, agent.ID, 10*time.Minute)

	h.checkPermissionKick()

	got, err := s.AgentGet(agent.ID)
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want running (auto-unpause after stream recovery)", got.Status)
	}
	awaitNoConnection(t, rec)
}

// Spawn-Parameter: permission_kick "on"/"off"/"" landen im Agent-Record.
func TestHandleSpawnAgent_PermissionKickParam(t *testing.T) {
	h, s := mustHandler(t)
	h.dataDir = t.TempDir()
	h.disableAgentProcesses = true
	h.agentDefaultBackend = "claude"

	resp := h.handleSpawnAgent(map[string]any{
		"project": "proj", "section": "pk-off", "permission_kick": "off",
		"work_dir": t.TempDir(),
	})
	if resp.Error != "" {
		t.Fatalf("spawn off: %s", resp.Error)
	}
	m := resultMap(t, resp)
	agent, err := s.AgentGet(m["id"].(string))
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if agent.PermissionKick != "off" {
		t.Errorf("PermissionKick = %q, want off", agent.PermissionKick)
	}

	resp = h.handleSpawnAgent(map[string]any{"project": "proj", "work_dir": t.TempDir(), "section": "pk-on", "permission_kick": "on"})
	if resp.Error != "" {
		t.Fatalf("spawn on: %s", resp.Error)
	}
	m = resultMap(t, resp)
	agent, err = s.AgentGet(m["id"].(string))
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if agent.PermissionKick != "on" {
		t.Errorf("PermissionKick = %q, want on", agent.PermissionKick)
	}

	resp = h.handleSpawnAgent(map[string]any{"project": "proj", "work_dir": t.TempDir(), "section": "pk-empty"})
	if resp.Error != "" {
		t.Fatalf("spawn empty: %s", resp.Error)
	}
	m = resultMap(t, resp)
	agent, err = s.AgentGet(m["id"].(string))
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if agent.PermissionKick != "" {
		t.Errorf("PermissionKick = %q, want empty (inherit)", agent.PermissionKick)
	}

	resp = h.handleSpawnAgent(map[string]any{"project": "proj", "work_dir": t.TempDir(), "section": "pk-bad", "permission_kick": "banana"})
	if resp.Error != "" {
		t.Fatalf("spawn invalid: %s", resp.Error)
	}
	m = resultMap(t, resp)
	agent, err = s.AgentGet(m["id"].(string))
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if agent.PermissionKick != "" {
		t.Errorf("invalid permission_kick value = %q, want empty (inherit)", agent.PermissionKick)
	}
}

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-274 [OPS-60]: agents.sock_path is writable through update_agent and
// register_agent, so the unlink of the socket files must not follow it. Every
// removal derives the path from the agent id instead and refuses an id that is
// not a plain, non-infrastructure name.
// These helpers and names are loop-specific on purpose: parallel yesloop runs
// share this package.

// los38eForeignSockFile creates a socket-path probe and its .inject sibling
// outside h.dataDir, standing in for a user file a caller points sock_path at.
func los38eForeignSockFile(t *testing.T) (sockPath string) {
	t.Helper()
	dir := t.TempDir()
	sockPath = filepath.Join(dir, "victim.sock")
	for _, p := range []string{sockPath, sockPath + ".inject"} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write probe %s: %v", p, err)
		}
	}
	return sockPath
}

// los38eOwnSockFile does the same inside the handler's own data directory, where
// a real agent socket lives.
func los38eOwnSockFile(t *testing.T, h *Handler) (sockPath string) {
	t.Helper()
	sockPath = filepath.Join(h.dataDir, "agent-1.sock")
	for _, p := range []string{sockPath, sockPath + ".inject"} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write probe %s: %v", p, err)
		}
	}
	return sockPath
}

func los38eAssertExists(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive: a caller-chosen sock_path is not the daemon's file to delete (%v)", p, err)
		}
	}
}

func los38eAssertGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s must be removed: the daemon's own socket files are its to clean up (stat err = %v)", p, err)
		}
	}
}

// handleUpdateAgent removes the socket files of an agent that reported
// finished/error. A row whose sock_path points outside the data directory must
// not turn that into a delete-anything primitive.
func TestSockPathOwnership_UpdateAgentKeepsForeignPath(t *testing.T) {
	h, s := mustHandler(t)
	sockPath := los38eForeignSockFile(t)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-foreign",
		Status: "running", Backend: "claude", SockPath: sockPath,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleUpdateAgent(map[string]any{
		"id":     "agent-1",
		"fields": map[string]any{"status": "finished"},
	})
	if resp.Error != "" {
		t.Fatalf("update_agent: %s", resp.Error)
	}

	los38eAssertExists(t, sockPath, sockPath+".inject")
}

func TestSockPathOwnership_UpdateAgentRemovesOwnPath(t *testing.T) {
	h, s := mustHandler(t)
	sockPath := los38eOwnSockFile(t, h)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-own",
		Status: "running", Backend: "claude", SockPath: sockPath,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleUpdateAgent(map[string]any{
		"id":     "agent-1",
		"fields": map[string]any{"status": "finished"},
	})
	if resp.Error != "" {
		t.Fatalf("update_agent: %s", resp.Error)
	}

	los38eAssertGone(t, sockPath, sockPath+".inject")
}

// stop_agent closes a row and cleans up its socket files. The same ownership
// rule applies: the row's sock_path came from a caller.
func TestSockPathOwnership_StopAgentKeepsForeignPath(t *testing.T) {
	h, s := mustHandler(t)
	sockPath := los38eForeignSockFile(t)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-foreign",
		Status: "running", Backend: "claude", SockPath: sockPath,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleStopAgent(map[string]any{"to": "agent-1"})
	if resp.Error != "" {
		t.Fatalf("stop_agent: %s", resp.Error)
	}

	los38eAssertExists(t, sockPath, sockPath+".inject")
}

func TestSockPathOwnership_StopAgentRemovesOwnPath(t *testing.T) {
	h, s := mustHandler(t)
	sockPath := los38eOwnSockFile(t, h)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-own",
		Status: "running", Backend: "claude", SockPath: sockPath,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleStopAgent(map[string]any{"to": "agent-1"})
	if resp.Error != "" {
		t.Fatalf("stop_agent: %s", resp.Error)
	}

	los38eAssertGone(t, sockPath, sockPath+".inject")
}

// stop_all_agents walks every stoppable row of a project in one call, so a
// single caller-chosen path there deletes without any per-agent decision.
func TestSockPathOwnership_StopAllAgentsKeepsForeignPath(t *testing.T) {
	h, s := mustHandler(t)
	sockPath := los38eForeignSockFile(t)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-foreign",
		Status: "running", Backend: "claude", SockPath: sockPath,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleStopAllAgents(map[string]any{"project": "proj"})
	if resp.Error != "" {
		t.Fatalf("stop_all_agents: %s", resp.Error)
	}

	los38eAssertExists(t, sockPath, sockPath+".inject")
}

func TestSockPathOwnership_StopAllAgentsRemovesOwnPath(t *testing.T) {
	h, s := mustHandler(t)
	sockPath := los38eOwnSockFile(t, h)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-own",
		Status: "running", Backend: "claude", SockPath: sockPath,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleStopAllAgents(map[string]any{"project": "proj"})
	if resp.Error != "" {
		t.Fatalf("stop_all_agents: %s", resp.Error)
	}

	los38eAssertGone(t, sockPath, sockPath+".inject")
}

// A path that reaches into the data directory through a traversal component is
// still outside it: the ownership check has to compare cleaned paths.
func TestSockPathOwnership_TraversalOutOfDataDirIsForeign(t *testing.T) {
	h, s := mustHandler(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "victim.sock")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	traversal := filepath.Join(h.dataDir, "..", filepath.Base(dir), "victim.sock")

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-traversal",
		Status: "running", Backend: "claude", SockPath: traversal,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleStopAgent(map[string]any{"to": "agent-1"})
	if resp.Error != "" {
		t.Fatalf("stop_agent: %s", resp.Error)
	}

	los38eAssertExists(t, target)
}

// The data directory is the daemon's whole state, not a socket pen: confining
// the unlink to it still leaves agents.db, config.yaml and the auth token one
// update_agent call away from deletion. The cleanup must derive the socket path
// from the agent id and ignore the column entirely.
func TestSockPathOwnership_DataDirStateFileSurvivesUpdateAgent(t *testing.T) {
	h, s := mustHandler(t)
	statePath := filepath.Join(h.dataDir, "agents.db")
	if err := os.WriteFile(statePath, []byte("state"), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-state",
		Status: "running", Backend: "claude", SockPath: statePath,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleUpdateAgent(map[string]any{
		"id":     "agent-1",
		"fields": map[string]any{"status": "finished"},
	})
	if resp.Error != "" {
		t.Fatalf("update_agent: %s", resp.Error)
	}

	los38eAssertExists(t, statePath)
}

func TestSockPathOwnership_DataDirStateFileSurvivesStopAgent(t *testing.T) {
	h, s := mustHandler(t)
	statePath := filepath.Join(h.dataDir, "config.yaml")
	if err := os.WriteFile(statePath, []byte("state"), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-state",
		Status: "running", Backend: "claude", SockPath: statePath,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleStopAgent(map[string]any{"to": "agent-1"})
	if resp.Error != "" {
		t.Fatalf("stop_agent: %s", resp.Error)
	}

	los38eAssertExists(t, statePath)
}

// The socket the daemon created for the agent is still cleaned up even when the
// row points elsewhere: the cleanup follows the id, not the column.
func TestSockPathOwnership_CleanupFollowsIDNotTheColumn(t *testing.T) {
	h, s := mustHandler(t)
	foreign := los38eForeignSockFile(t)
	own := los38eOwnSockFile(t, h)

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec-mismatch",
		Status: "running", Backend: "claude", SockPath: foreign,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleStopAgent(map[string]any{"to": "agent-1"})
	if resp.Error != "" {
		t.Fatalf("stop_agent: %s", resp.Error)
	}

	los38eAssertExists(t, foreign, foreign+".inject")
	los38eAssertGone(t, own, own+".inject")
}

// The derived path is only as good as the id that names it: a separator in the
// id walks the unlink through a symlinked directory that sits inside dataDir,
// and the lexical prefix check cannot see it.
func TestSockPathOwnership_SymlinkedDirCannotCarryIDOut(t *testing.T) {
	h, s := mustHandler(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.sock")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(h.dataDir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := s.AgentCreate(storage.Agent{
		ID: "link/victim", Project: "proj", Section: "sec-symlink",
		Status: "running", Backend: "claude",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleStopAgent(map[string]any{"to": "link/victim"})
	if resp.Error != "" {
		t.Fatalf("stop_agent: %s", resp.Error)
	}

	los38eAssertExists(t, victim)
}

// An id that is not a plain name must not reach the daemon's own infrastructure
// sockets either.
func TestSockPathOwnership_ReservedDaemonSocketSurvives(t *testing.T) {
	for _, tc := range []struct{ id, file string }{
		{"daemon", "daemon.sock"},
		{"job", "job.sock"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			h, s := mustHandler(t)
			reserved := filepath.Join(h.dataDir, tc.file)
			if err := os.WriteFile(reserved, []byte("x"), 0o600); err != nil {
				t.Fatalf("write probe: %v", err)
			}

			if err := s.AgentCreate(storage.Agent{
				ID: tc.id, Project: "proj", Section: "sec-reserved",
				Status: "running", Backend: "claude",
			}); err != nil {
				t.Fatalf("AgentCreate: %v", err)
			}

			resp := h.handleStopAgent(map[string]any{"to": tc.id})
			if resp.Error != "" {
				t.Fatalf("stop_agent: %s", resp.Error)
			}

			los38eAssertExists(t, reserved)
		})
	}
}

// The cascade closes child rows whose processes are already gone. Their sockets
// are cleaned up by id, but only for a row that ever bound a socket — a row that
// never did must not take a successor's file with it.
func TestSockPathOwnership_CascadeCleansOnlyRowsThatBoundASocket(t *testing.T) {
	h, _ := mustHandler(t)
	bound := filepath.Join(h.dataDir, "child-bound.sock")
	neverBound := filepath.Join(h.dataDir, "child-never.sock")
	for _, p := range []string{bound, bound + ".inject", neverBound, neverBound + ".inject"} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write probe %s: %v", p, err)
		}
	}

	h.endCascadedProcesses([]storage.Agent{
		{ID: "child-bound", Section: "s1", SockPath: bound},
		{ID: "child-never", Section: "s2"},
	})

	los38eAssertGone(t, bound, bound+".inject")
	los38eAssertExists(t, neverBound, neverBound+".inject")
}

// A spawn creates the socket file before it writes sock_path, so a row with an
// empty column can still be a spawn in flight. Cleanup therefore requires the
// row to have bound a socket, at every site that closes a row.
func TestSockPathOwnership_UnboundRowKeepsInFlightSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(h *Handler, id string) Response
	}{
		{
			name: "update_agent finished",
			stop: func(h *Handler, id string) Response {
				return h.handleUpdateAgent(map[string]any{
					"id":     id,
					"fields": map[string]any{"status": "finished"},
				})
			},
		},
		{
			name: "stop_agent",
			stop: func(h *Handler, id string) Response { return h.handleStopAgent(map[string]any{"to": id}) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := mustHandler(t)
			own := filepath.Join(h.dataDir, "spawn-in-flight.sock")
			if err := os.WriteFile(own, []byte("x"), 0o600); err != nil {
				t.Fatalf("write probe: %v", err)
			}
			if err := s.AgentCreate(storage.Agent{
				ID: "spawn-in-flight", Project: "proj", Section: "sec-unbound",
				Status: "spawning", Backend: "claude",
			}); err != nil {
				t.Fatalf("AgentCreate: %v", err)
			}

			if resp := tc.stop(h, "spawn-in-flight"); resp.Error != "" {
				t.Fatalf("%s: %s", tc.name, resp.Error)
			}

			los38eAssertExists(t, own)
		})
	}
}

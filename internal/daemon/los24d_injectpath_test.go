package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los24dAgentSock points the handler at a short socket directory and returns the
// inject-socket base path the daemon derives for id. Tests that used to place a
// listener at a free socket path use this instead: since YM-338 the column is no
// longer a dial target.
func los24dAgentSock(t *testing.T, h *Handler, id string) string {
	t.Helper()
	dir := shortSockDir(t)
	h.dataDir = dir
	return filepath.Join(dir, id+".sock")
}

// YM-338 (Los 24d): every relay and every termination hint dialed
// agents.sock_path, a column that update_agent and register_agent write verbatim.
// A caller could therefore point a guard relay, a permission kick or a stop's
// /exit at a foreign socket. The dial target is derived from the agent id, like
// the unlink paths already are (YM-274/YM-287).
//
// Each case listens on both paths: the derived one below h.dataDir and a foreign
// one in its own directory. Only the derived one may see a connection.
func TestAgentRelaysIgnoreTheSockPathColumn(t *testing.T) {
	guardResult := ValidationResult{Compliant: false, PhaseCount: 3, MissingPhases: []int{5}}

	for _, tc := range []struct {
		name  string
		want  int
		drive func(h *Handler, agent storage.Agent)
	}{
		{
			name: "relay_agent",
			want: 1,
			drive: func(h *Handler, agent storage.Agent) {
				resp := h.handleRelayAgent(map[string]any{"to": agent.ID, "content": "go on"})
				if resp.Error != "" {
					t.Errorf("relay_agent: %s", resp.Error)
				}
			},
		},
		{
			name: "done_guard",
			want: 1,
			drive: func(h *Handler, agent storage.Agent) {
				h.sendDoneGuardRelay(agent, guardResult)
			},
		},
		{
			name: "stagnation",
			want: 1,
			drive: func(h *Handler, agent storage.Agent) {
				h.sendStagnationRelay(agent, buildStagnationRelayMessage(20*time.Minute))
			},
		},
		{
			name: "done_verify",
			want: 1,
			drive: func(h *Handler, agent storage.Agent) {
				h.sendDoneVerifyRelay(agent)
			},
		},
		{
			name: "skillcheck",
			want: 1,
			drive: func(h *Handler, agent storage.Agent) {
				h.sendSkillCheckRelay(agent, 1)
			},
		},
		{
			name: "permission_kick",
			want: 2, // two separate enters, see sendPermissionKick
			drive: func(h *Handler, agent storage.Agent) {
				h.sendPermissionKick(agent)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := mustHandler(t)
			h.dataDir = shortSockDir(t)
			id := "agent-1"
			derived := filepath.Join(h.dataDir, id+".sock")
			own := startFakeInjectSocket(t, derived+".inject")

			foreignDir := shortSockDir(t)
			foreign := filepath.Join(foreignDir, "victim.sock")
			foreignRec := startFakeInjectSocket(t, foreign+".inject")

			agent := storage.Agent{
				ID: id, Project: "proj", Section: "sec-" + tc.name,
				Status: "running", Backend: "opencode",
				SessionID: "sess-" + tc.name,
				SockPath:  foreign,
			}
			if err := s.AgentCreate(agent); err != nil {
				t.Fatalf("AgentCreate: %v", err)
			}

			tc.drive(h, agent)

			waitForConnections(t, own, tc.want)
			awaitNoConnection(t, foreignRec)
		})
	}
}

// The derived path is the only dial target, so an agent whose column is empty is
// still relayable: the column says nothing about the socket the daemon created.
func TestAgentRelayWorksWithAnEmptySockPathColumn(t *testing.T) {
	h, s := mustHandler(t)
	h.dataDir = shortSockDir(t)
	id := "agent-1"
	own := startFakeInjectSocket(t, filepath.Join(h.dataDir, id+".sock.inject"))

	agent := storage.Agent{
		ID: id, Project: "proj", Section: "sec-empty",
		Status: "running", Backend: "opencode", SessionID: "sess-empty",
	}
	if err := s.AgentCreate(agent); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	if resp := h.handleRelayAgent(map[string]any{"to": id, "content": "go on"}); resp.Error != "" {
		t.Fatalf("relay_agent with an empty sock_path column: %s", resp.Error)
	}
	waitForConnections(t, own, 1)
}

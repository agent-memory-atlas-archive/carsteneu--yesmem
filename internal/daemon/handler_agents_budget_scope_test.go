package daemon

import "testing"

// TestHandleOpenAgentTerminal_NoTokenBudget pins the boundary of YM-67: the
// documented 500000 default belongs to spawned agents. The heartbeat enforces a
// budget by stopping the agent, and this row backs a window a human works in — a
// default here would end an interactive session mid-work.
func TestHandleOpenAgentTerminal_NoTokenBudget(t *testing.T) {
	h, s := mustHandler(t)

	resp := h.handleOpenAgentTerminal(map[string]any{
		"project":  "proj",
		"work_dir": t.TempDir(),
		"backend":  "opencode",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	id, _ := resultMap(t, resp)["agent_id"].(string)
	if id == "" {
		t.Fatalf("no agent id in response")
	}

	agent, err := s.AgentGet(id)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if agent.TokenBudget != 0 {
		t.Fatalf("TokenBudget = %d, want 0 (interactive terminal sessions stay unlimited)", agent.TokenBudget)
	}
}

// TestHandleSpawnAgent_StillNoDefault keeps the other side: a spawned agent without
// a request stays unlimited (D18 — the limit is opt-in, not a default).
func TestHandleSpawnAgent_StillNoDefault(t *testing.T) {
	h, s := mustHandler(t)
	resp := h.handleSpawnAgent(map[string]any{"project": "proj", "work_dir": t.TempDir(), "section": "task-default-again"})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	id, _ := resultMap(t, resp)["id"].(string)
	agent, err := s.AgentGet(id)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if agent.TokenBudget != 0 {
		t.Fatalf("TokenBudget = %d, want 0 (no limit)", agent.TokenBudget)
	}
}

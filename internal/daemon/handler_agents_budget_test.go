package daemon

import "testing"

// D18: token_budget 0 or unset means NO limit. The audit finding YM-67 was a
// documentation contradiction (config.yaml claimed a 500000 default that nothing
// applied), and the correction is the documentation, not a default — input_tokens
// is cumulative including cache reads, so a 500000 default would stop every agent
// after a few turns.
func TestHandleSpawnAgent_NoBudgetMeansNoLimit(t *testing.T) {
	h, s := mustHandler(t) // agentTokenBudget stays 0 = not configured

	resp := h.handleSpawnAgent(map[string]any{"project": "proj", "work_dir": t.TempDir(), "section": "task-no-budget"})
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

// TestHandleSpawnAgent_ExplicitZeroMeansNoLimit: the MCP schema says 0 is "no limit",
// neutral value, and it stays neutral — it is not a request for a default.
func TestHandleSpawnAgent_ExplicitZeroMeansNoLimit(t *testing.T) {
	h, s := mustHandler(t)

	resp := h.handleSpawnAgent(map[string]any{
		"project": "proj", "section": "task-zero-budget", "token_budget": float64(0),
		"work_dir": t.TempDir(),
	})
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

// TestHandleSpawnAgent_ConfiguredBudgetWins: an agent token_budget > 0 in the
// config is a real limit and applies to a spawn that does not name one.
func TestHandleSpawnAgent_ConfiguredBudgetWins(t *testing.T) {
	h, s := mustHandler(t)
	h.agentTokenBudget = 12345

	resp := h.handleSpawnAgent(map[string]any{"project": "proj", "work_dir": t.TempDir(), "section": "task-budget-cfg"})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	id, _ := resultMap(t, resp)["id"].(string)
	agent, err := s.AgentGet(id)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if agent.TokenBudget != 12345 {
		t.Fatalf("TokenBudget = %d, want the configured 12345", agent.TokenBudget)
	}
}

// TestHandleSpawnAgent_ExplicitBudgetWins: a per-spawn value beats the config.
func TestHandleSpawnAgent_ExplicitBudgetWins(t *testing.T) {
	h, s := mustHandler(t)
	h.agentTokenBudget = 12345

	resp := h.handleSpawnAgent(map[string]any{
		"project": "proj", "section": "task-budget-exp", "token_budget": float64(777),
		"work_dir": t.TempDir(),
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	id, _ := resultMap(t, resp)["id"].(string)
	agent, err := s.AgentGet(id)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if agent.TokenBudget != 777 {
		t.Fatalf("TokenBudget = %d, want the explicit 777", agent.TokenBudget)
	}
}

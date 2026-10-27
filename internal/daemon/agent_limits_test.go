package daemon

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// TestEnforceAgentLimits_NoBudgetIsNeverStopped pins D18 from the enforcement side:
// an agent row without a budget is not stopped, however many tokens it has burned.
// input_tokens is cumulative including cache reads (live YesLoops sit at 50 to 130
// million), so any default here would stop the whole next wave after a deploy.
func TestEnforceAgentLimits_NoBudgetIsNeverStopped(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-nolimit", Project: "proj", Section: "sec-nolimit",
		SessionID: "uuid-nolimit", PID: 0, Status: "running", Backend: "claude",
		TokenBudget: 0,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if err := s.AgentUpdateTelemetry("agent-nolimit", 0, 128_588_001, 0); err != nil {
		t.Fatalf("AgentUpdateTelemetry: %v", err)
	}

	h.enforceAgentLimits()

	got, err := s.AgentGet("agent-nolimit")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status != "running" {
		t.Fatalf("status = %q, want running: an agent without a budget has no limit", got.Status)
	}
}

// TestEnforceAgentLimits_ExplicitBudgetIsEnforced: a real budget still stops the
// agent. This is the other half of the contract D18 keeps.
func TestEnforceAgentLimits_ExplicitBudgetIsEnforced(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-limited", Project: "proj", Section: "sec-limited",
		SessionID: "uuid-limited", PID: 0, Status: "running", Backend: "claude",
		TokenBudget: 1000,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	// Output tokens are the budget's unit, so the output figure has to exceed it.
	if err := s.AgentUpdateTelemetry("agent-limited", 0, 0, 2000); err != nil {
		t.Fatalf("AgentUpdateTelemetry: %v", err)
	}
	// The row was just created, so the runtime limit cannot be the reason it stops.
	h.enforceAgentLimits()

	got, err := s.AgentGet("agent-limited")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status != "stopped" {
		t.Fatalf("status = %q, want stopped: token_budget 1000 was exceeded", got.Status)
	}
	// The reason matters: a max_runtime or max_turns stop would also flip the status.
	if !strings.Contains(got.Progress, "token_budget 1000 exceeded") {
		t.Errorf("progress = %q, want it to name the budget as the reason", got.Progress)
	}
}

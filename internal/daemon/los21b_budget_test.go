package daemon

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-254: the budget compared input_tokens+output_tokens against token_budget.
// input_tokens is cumulative and includes cache reads — live YesLoops sit at 50
// to 130 million at 300 to 750 turns — so a modest budget ended agents that had
// barely produced anything. The budget now counts output tokens, the model's own
// work: a cache-heavy input volume no longer trips it.
func TestEnforceAgentLimits_BudgetIgnoresCachedInputVolume(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-cache-heavy", Project: "proj", Section: "sec-cache-heavy",
		SessionID: "uuid-cache-heavy", PID: 0, Status: "running", Backend: "claude",
		TokenBudget: 1000,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	// 300 turns of a large cached context that produced 100 output tokens.
	if err := s.AgentUpdateTelemetry("agent-cache-heavy", 300, 128_000_000, 100); err != nil {
		t.Fatalf("AgentUpdateTelemetry: %v", err)
	}

	h.enforceAgentLimits()

	got, err := s.AgentGet("agent-cache-heavy")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status != "running" {
		t.Fatalf("status = %q, want running: cached input volume is not work and must not exhaust a budget measured in output tokens", got.Status)
	}
}

// The other half: output tokens still count, and the row names the measure so an
// operator can tell which unit the budget is in.
func TestEnforceAgentLimits_BudgetCountsOutputTokens(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-output-heavy", Project: "proj", Section: "sec-output-heavy",
		SessionID: "uuid-output-heavy", PID: 0, Status: "running", Backend: "claude",
		TokenBudget: 1000,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if err := s.AgentUpdateTelemetry("agent-output-heavy", 50, 500, 1200); err != nil {
		t.Fatalf("AgentUpdateTelemetry: %v", err)
	}

	h.enforceAgentLimits()

	got, err := s.AgentGet("agent-output-heavy")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status != "stopped" {
		t.Fatalf("status = %q, want stopped: 1200 output tokens exceed the budget of 1000", got.Status)
	}
	if !strings.Contains(got.Progress, "output tokens") {
		t.Errorf("progress = %q, want the measure named (output tokens)", got.Progress)
	}
}

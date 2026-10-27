package extraction

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/sanitize"
)

// The daemon wraps every LLM handle with a SanitizingClient as the OUTERMOST
// decorator (sanitizing_client.go). SetMaxBudgetPerCall has to see through that
// wrapper, otherwise the per-call cap is silently dropped on every handle that
// carries a redactor.

func TestSetMaxBudgetPerCall_SeesThroughSanitizingClient(t *testing.T) {
	cli := NewCLIClient("claude", "test-model", "claude")
	wrapped := NewSanitizingClient(cli, sanitize.NewSecretRedactor(nil))

	SetMaxBudgetPerCall(wrapped, 1.25)

	if cli.MaxBudgetPerCall != 1.25 {
		t.Fatalf("MaxBudgetPerCall = %v, want 1.25 — the SanitizingClient wrapper hid the CLI client from SetMaxBudgetPerCall", cli.MaxBudgetPerCall)
	}
}

func TestSetMaxBudgetPerCall_SeesThroughFullDaemonChain(t *testing.T) {
	cli := NewCLIClient("claude", "test-model", "claude")
	tracker := NewBudgetTracker(10)
	budgeted := NewBudgetClient(cli, tracker)
	gated := NewGatedClient(budgeted, NewAPIGate(tracker, nil, 5, 0.50), "extract")
	wrapped := NewSanitizingClient(gated, sanitize.NewSecretRedactor(nil))

	SetMaxBudgetPerCall(wrapped, 2.50)

	if cli.MaxBudgetPerCall != 2.50 {
		t.Fatalf("MaxBudgetPerCall = %v, want 2.50 — raw -> BudgetClient -> GatedClient -> SanitizingClient must still reach the CLI client", cli.MaxBudgetPerCall)
	}
}

func TestSetMaxBudgetPerCall_StillReachesUnwrappedCLIClient(t *testing.T) {
	cli := NewCLIClient("claude", "test-model", "claude")
	SetMaxBudgetPerCall(cli, 0.75)
	if cli.MaxBudgetPerCall != 0.75 {
		t.Fatalf("MaxBudgetPerCall = %v, want 0.75", cli.MaxBudgetPerCall)
	}
}

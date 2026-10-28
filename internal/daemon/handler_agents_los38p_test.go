package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/orchestrator"
	"github.com/carsteneu/yesmem/internal/storage"
)

// --- Los 38p (YM-335): agents.sock_path ueber update_agent ---

// TestUpdateAgent_Los38p_RejectsForeignSockPath: the column is the dial target of
// every guard relay and the precondition of the idle guard's unreadable hold, so
// a socket caller that can write it redirects its own deliveries and widens its
// own supervision window. The daemon derives the path from the agent id; a caller
// has no reason to name a different one. The empty value stays accepted because a
// resume writes it to take the row over.
func TestUpdateAgent_Los38p_RejectsForeignSockPath(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-1", Project: "proj", Section: "sec",
		SessionID: "sess-1", Status: "running", Backend: "claude",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleUpdateAgent(map[string]any{
		"id":     "agent-1",
		"fields": map[string]any{"sock_path": "/tmp/attacker.sock"},
	})
	if resp.Error == "" {
		t.Fatal("update_agent accepted a caller-chosen socket path")
	}
	row, err := s.AgentGet("agent-1")
	if err != nil || row == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if row.SockPath != "" {
		t.Fatalf("sock_path = %q, want the rejected write to leave the column alone", row.SockPath)
	}

	// A value that is not a string is refused as well: stored as it arrived, the
	// row would carry a value no later read can bind.
	if resp := h.handleUpdateAgent(map[string]any{
		"id":     "agent-1",
		"fields": map[string]any{"sock_path": float64(7)},
	}); resp.Error == "" {
		t.Fatal("update_agent accepted a non-string socket path")
	}

	// Both values the daemon itself writes stay accepted: the path it derived for
	// the row, and the empty one a resume uses.
	derived := h.agentSockPath("agent-1")
	if derived == "" {
		t.Fatal("precondition: the handler derived no socket path for the row")
	}
	if resp := h.handleUpdateAgent(map[string]any{
		"id":     "agent-1",
		"fields": map[string]any{"sock_path": derived},
	}); resp.Error != "" {
		t.Fatalf("update_agent rejected the daemon-derived path %q: %s", derived, resp.Error)
	}
	if resp := h.handleUpdateAgent(map[string]any{
		"id":     "agent-1",
		"fields": map[string]any{"sock_path": ""},
	}); resp.Error != "" {
		t.Fatalf("update_agent rejected the empty socket path a resume writes: %s", resp.Error)
	}
}

// TestHandleResumeAgent_Los38p_TestGuardStopsTheSpawn: the resume path was the
// one spawn route that ignored disableAgentProcesses, the guard the three other
// routes carry. In the suite that means the background goroutine really starts the
// backend (pty.Start of claude) and then a terminal window, and it keeps writing
// into the test's store and temp dir after the test returned — the load-dependent
// flake of TestHandleResumeAgent_FinishedIsResumable.
func TestHandleResumeAgent_Los38p_TestGuardStopsTheSpawn(t *testing.T) {
	h, s := mustHandler(t)
	if !h.disableAgentProcesses {
		t.Fatal("precondition: the test handler must carry the process guard")
	}
	createRow := func() {
		t.Helper()
		if err := s.AgentCreate(storage.Agent{
			ID: "agent-1", Project: "proj", Section: "task",
			SessionID: "sess-1", Status: "finished", Backend: "claude",
		}); err != nil {
			t.Fatalf("AgentCreate: %v", err)
		}
	}
	createRow()

	spawned := make(chan string, 1)
	prev := newAgentBridge
	newAgentBridge = func(name string, args []string, sock, workDir string, extraEnv ...string) (*orchestrator.AgentBridge, error) {
		spawned <- name
		return nil, fmt.Errorf("no bridge in this test")
	}
	t.Cleanup(func() { newAgentBridge = prev })

	// Positive control: with the guard down the seam fires. Without it the negative
	// half below would also pass if the stub were wired wrong.
	h.disableAgentProcesses = false
	if resp := h.handleResumeAgent(map[string]any{"to": "agent-1"}); resp.Error != "" {
		t.Fatalf("finished agents should be resumable, got error: %s", resp.Error)
	}
	select {
	case <-spawned:
	case <-time.After(2 * time.Second):
		t.Fatal("the stubbed bridge was never reached although the guard was down")
	}

	// The failed bridge writes its error from the spawn goroutine, so the
	// receive above only proves the seam was entered, not that the row is
	// settled. Resetting before that write lands lets it flip the row back to
	// "error" afterwards, and the negative half then judged the goroutine's
	// leftover instead of the guard (YM-408, the load-dependent flake of this
	// test). Waiting for the recorded failure is the event that makes the reset
	// deterministic; the deadline only bounds the wait. "error" is unambiguous
	// here: the row starts "finished", the resume above moves it to "pending",
	// and the failed bridge is the only writer left.
	deadline := time.Now().Add(5 * time.Second)
	for {
		row, err := s.AgentGet("agent-1")
		if err != nil {
			t.Fatalf("AgentGet: %v", err)
		}
		if row.Status == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the failed spawn never recorded its error on the row: status=%q error=%q", row.Status, row.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The failed bridge closed the row; a fresh one makes the negative half judge
	// the guard, not the row state.
	if err := s.AgentUpdate("agent-1", map[string]any{"status": "finished", "error": "", "sock_path": ""}); err != nil {
		t.Fatalf("reset the row: %v", err)
	}
	h.disableAgentProcesses = true
	if resp := h.handleResumeAgent(map[string]any{"to": "agent-1"}); resp.Error != "" {
		t.Fatalf("finished agents should be resumable, got error: %s", resp.Error)
	}

	select {
	case name := <-spawned:
		t.Fatalf("resume started the backend %q although the process guard is set: the suite would run a real agent and a terminal window", name)
	case <-time.After(500 * time.Millisecond):
	}
}

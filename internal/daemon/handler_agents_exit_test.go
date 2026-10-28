package daemon

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// TestHandleUpdateAgent_ExpectedPIDRefusesForeignRow pins YM-202: an exit write
// that names the PID it came from must not land on a row a successor has bound in
// the meantime. The CLI spawn path called update_agent unconditionally, so a
// finished agent could overwrite the status, the PID and the sockets of the agent
// that replaced it.
func TestHandleUpdateAgent_ExpectedPIDRefusesForeignRow(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-exit-1", Project: "proj", Section: "sec-exit",
		SessionID: "uuid-exit", PID: 111, Status: "running",
		Backend: "claude", SockPath: "/tmp/successor.sock",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleUpdateAgent(map[string]any{
		"id":           "agent-exit-1",
		"expected_pid": float64(222), // a successor bound this PID
		"fields":       map[string]any{"status": "error", "error": "exit: 1", "stopped_at": "2026-10-17T10:00:00Z"},
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if written, ok := resultMap(t, resp)["written"]; !ok || written != false {
		t.Errorf("response written = %v, want false (the row is not ours)", written)
	}

	got, err := s.AgentGet("agent-exit-1")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want running: the successor's row was overwritten", got.Status)
	}
	if got.PID != 111 {
		t.Errorf("pid = %d, want 111: the successor's binding was erased", got.PID)
	}
	if got.SockPath != "/tmp/successor.sock" {
		t.Errorf("sock_path = %q, want the successor's socket", got.SockPath)
	}
}

// TestHandleUpdateAgent_ExpectedPIDOwnsTheRow: the same call with the PID the row
// actually names records the exit and clears the binding, so the caller knows the
// sockets are its own to remove.
func TestHandleUpdateAgent_ExpectedPIDOwnsTheRow(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-exit-2", Project: "proj", Section: "sec-exit-2",
		SessionID: "uuid-exit-2", PID: 333, Status: "running",
		Backend: "claude", SockPath: "/tmp/own.sock",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleUpdateAgent(map[string]any{
		"id":           "agent-exit-2",
		"expected_pid": float64(333),
		"fields":       map[string]any{"status": "finished", "stopped_at": "2026-10-17T10:00:00Z"},
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if written, _ := resultMap(t, resp)["written"].(bool); !written {
		t.Fatalf("response written = false, want true for the owning PID")
	}

	got, err := s.AgentGet("agent-exit-2")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status != "finished" {
		t.Errorf("status = %q, want finished", got.Status)
	}
	if got.PID != 0 || got.SockPath != "" {
		t.Errorf("binding = (pid %d, sock %q), want it cleared by the exit", got.PID, got.SockPath)
	}
}

// TestHandleUpdateAgent_ExpectedPIDRejectsForeignFields: the exit statement only
// moves status, error and stopped_at. A key it cannot move must be refused, not
// dropped silently.
func TestHandleUpdateAgent_ExpectedPIDRejectsForeignFields(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-exit-3", Project: "proj", Section: "sec-exit-3",
		SessionID: "uuid-exit-3", PID: 444, Status: "running", Backend: "claude",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleUpdateAgent(map[string]any{
		"id":           "agent-exit-3",
		"expected_pid": float64(444),
		"fields":       map[string]any{"status": "finished", "progress": "sneaky"},
	})
	if resp.Error == "" {
		t.Fatalf("foreign field accepted on the exit path")
	}

	got, _ := s.AgentGet("agent-exit-3")
	if got.Status != "running" {
		t.Errorf("status = %q, want running: a refused write must not land", got.Status)
	}
}

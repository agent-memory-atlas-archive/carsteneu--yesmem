package storage

import "testing"

// TestLos38R_AgentUpdateRefusesSessionID: session_id is what whoami, send_to and
// the yesloop pause gate follow for a row, so it must not be writable through
// the generic update — a socket caller could otherwise point its own row at
// another session and freeze the guard that is supposed to pause it (YM-344).
// The row is written once by AgentCreate and never through AgentUpdate.
func TestLos38R_AgentUpdateRefusesSessionID(t *testing.T) {
	s := newTestStore(t)
	createAgentForBind(t, s, "a38r", "")

	if err := s.AgentUpdate("a38r", map[string]any{"session_id": "planted-session"}); err == nil {
		t.Fatal("session_id must not be settable through AgentUpdate")
	}
	a, err := s.AgentGet("a38r")
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if a.SessionID != "uuid-a38r" {
		t.Errorf("session_id = %q, want the row untouched", a.SessionID)
	}
}

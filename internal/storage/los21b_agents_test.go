package storage

import "testing"

// AgentClosePausedIfDead is the predicate the spawn path uses to free a section
// whose pause has no process: the row must still be paused AND still carry the PID
// the decision was made on. A resume writes "pending" before it binds a process, so
// a PID-only predicate could overwrite a takeover already under way and two
// processes would end up owning one section (YM-255 review).
func TestAgentClosePausedIfDead_RequiresPausedStatusAndPID(t *testing.T) {
	s := newTestStore(t)
	if err := s.AgentCreate(Agent{
		ID: "los21b-paused", Project: "p", Section: "s", Status: "paused", PID: 0,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	closed, err := s.AgentClosePausedIfDead("los21b-paused", 0, "2026-10-17 23:00:00", "auto-stopped: process gone")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if !closed {
		t.Fatal("a paused row with the expected PID was not closed")
	}
	got, err := s.AgentGet("los21b-paused")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "stopped" || got.StoppedAt == "" {
		t.Errorf("row = status %q, stopped_at %q; want stopped with a stamp", got.Status, got.StoppedAt)
	}

	// The row is no longer paused: a second closure must not match, or a second
	// spawn on the same section would read the row as freshly freed.
	if closed, err := s.AgentClosePausedIfDead("los21b-paused", 0, "2026-10-17 23:00:01", "again"); err != nil || closed {
		t.Fatalf("second close = (%v, %v), want not written", closed, err)
	}

	// A resume that took the row over (status pending, PID not yet bound) wins.
	if err := s.AgentCreate(Agent{
		ID: "los21b-resumed", Project: "p", Section: "s2", Status: "paused", PID: 0,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if err := s.AgentUpdate("los21b-resumed", map[string]any{"status": "pending"}); err != nil {
		t.Fatalf("AgentUpdate: %v", err)
	}
	if closed, err := s.AgentClosePausedIfDead("los21b-resumed", 0, "2026-10-17 23:00:02", "auto-stopped"); err != nil || closed {
		t.Fatalf("close of a resumed row = (%v, %v), want not written", closed, err)
	}

	// A PID that moved must not match.
	if err := s.AgentCreate(Agent{
		ID: "los21b-pid", Project: "p", Section: "s3", Status: "paused", PID: 4242,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if closed, err := s.AgentClosePausedIfDead("los21b-pid", 111, "2026-10-17 23:00:03", "auto-stopped"); err != nil || closed {
		t.Fatalf("close with a stale PID = (%v, %v), want not written", closed, err)
	}
}

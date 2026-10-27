package mcp

import "testing"

// YM-172: the schedule tool accepted a backend parameter in the daemon but never
// declared it, so an agent could not discover that agent/headless jobs can pick
// their backend.
func TestScheduleToolExposesBackend(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	tool := s.srv.GetTool("schedule")
	if tool == nil {
		t.Fatal("GetTool(schedule) returned nil, tool not registered")
	}

	props := tool.Tool.InputSchema.Properties
	if _, ok := props["backend"]; !ok {
		t.Fatalf("schedule tool schema missing 'backend' property; have keys: %v", keysOf(props))
	}
}

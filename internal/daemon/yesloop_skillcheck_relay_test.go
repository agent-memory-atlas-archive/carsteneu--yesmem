package daemon


import (
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// startInjectSocketListener starts a unix listener on injectPath that counts
// every incoming connection. Each skill-check relay is a single dial+write to
// <sock_path>.inject, so the accept count is the relay count.
func startInjectSocketListener(t *testing.T, injectPath string) *int64 {
	t.Helper()
	ln, err := net.Listen("unix", injectPath)
	if err != nil {
		t.Fatalf("listen inject socket: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	var relays int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt64(&relays, 1)
			c.Close()
		}
	}()
	return &relays
}

// getSkillCheckLastRelayAt reads an agent's lastRelayAt (test helper).
func getSkillCheckLastRelayAt(agentID string) time.Time {
	yesloopSkillCheckAgentsMu.Lock()
	defer yesloopSkillCheckAgentsMu.Unlock()
	st, ok := yesloopSkillCheckAgents[agentID]
	if !ok {
		return time.Time{}
	}
	return st.lastRelayAt
}

// TestSkillCheck_Freeze_WhileSubagentActive: while the agent has an active
// opencode child session (task() subagent), the refire clock must freeze —
// no relay, no refire count, and the interval restarts after unfreeze.
func TestSkillCheck_Freeze_WhileSubagentActive(t *testing.T) {
	resetYesloopSkillCheckState()
	origFn := opencodeChildActiveFn
	var childActive bool
	opencodeChildActiveFn = func(string) bool { return childActive }
	t.Cleanup(func() { opencodeChildActiveFn = origFn })

	h, s := mustHandler(t)
	dir := shortSockDir(t)
	sockPath := filepath.Join(dir, "sc-freeze.sock")
	relays := startInjectSocketListener(t, sockPath+".inject")

	makeSkillCheckAgent(t, h, s, "sc-freeze", "sess-sc-freeze", "")
	if err := s.AgentUpdate("sc-freeze", map[string]any{"sock_path": sockPath}); err != nil {
		t.Fatalf("AgentUpdate sock_path: %v", err)
	}

	h.checkYesloopSkillCheck() // TRACKING, baseline 0
	jumpSkillCheckCounters(t, s, "sc-freeze", skillCheckOutputTokenThreshold, 0)
	h.checkYesloopSkillCheck() // -> REMIND, transition relay #1
	if !hasSkillCheckState("sc-freeze", yesloopSkillCheckStateRemind) {
		t.Fatalf("precondition: agent should be in REMIND")
	}
	time.Sleep(30 * time.Millisecond) // let the listener accept
	if got := atomic.LoadInt64(relays); got != 1 {
		t.Fatalf("precondition: expected 1 transition relay, got %d", got)
	}

	// Subagent active: even a fully elapsed interval must not refire; the
	// clock is shifted forward every frozen tick.
	childActive = true
	setSkillCheckLastRelayAt("sc-freeze", time.Now().Add(-2*yesloopSkillCheckRefireInterval))
	for i := 0; i < 5; i++ {
		h.checkYesloopSkillCheck()
		time.Sleep(20 * time.Millisecond)
	}
	if got := atomic.LoadInt64(relays); got != 1 {
		t.Errorf("subagent-active ticks must not relay, got %d extra", got-1)
	}

	// Unfreeze: interval restarts from the last frozen tick — no refire burst.
	childActive = false
	h.checkYesloopSkillCheck()
	time.Sleep(20 * time.Millisecond)
	if rc := getSkillCheckRefireCount("sc-freeze"); rc != 0 {
		t.Errorf("interval clock was not frozen during subagent: refireCount=%d", rc)
	}
	if got := atomic.LoadInt64(relays); got != 1 {
		t.Errorf("unfreeze tick must not relay, got %d total", got)
	}
}

// TestSkillCheck_Freeze_BlocksTransitionAndRelay: a threshold crossing while a
// subagent is active must neither transition to REMIND nor relay. The machine
// catches up on the first tick after the subagent returns.
func TestSkillCheck_Freeze_BlocksTransitionAndRelay(t *testing.T) {
	resetYesloopSkillCheckState()
	origFn := opencodeChildActiveFn
	var childActive bool
	opencodeChildActiveFn = func(string) bool { return childActive }
	t.Cleanup(func() { opencodeChildActiveFn = origFn })

	h, s := mustHandler(t)
	dir := shortSockDir(t)
	sockPath := filepath.Join(dir, "sc-blocked.sock")
	relays := startInjectSocketListener(t, sockPath+".inject")

	makeSkillCheckAgent(t, h, s, "sc-blocked", "sess-sc-blocked", "")
	if err := s.AgentUpdate("sc-blocked", map[string]any{"sock_path": sockPath}); err != nil {
		t.Fatalf("AgentUpdate sock_path: %v", err)
	}

	childActive = true
	h.checkYesloopSkillCheck() // TRACKING, baseline 0
	jumpSkillCheckCounters(t, s, "sc-blocked", skillCheckOutputTokenThreshold, 0)
	h.checkYesloopSkillCheck() // threshold crossed, but frozen
	if !hasSkillCheckState("sc-blocked", yesloopSkillCheckStateTracking) {
		t.Errorf("subagent-active must block transition to REMIND, got state %d", stateOfSkillCheck("sc-blocked"))
	}
	if got := atomic.LoadInt64(relays); got != 0 {
		t.Errorf("subagent-active must not relay, got %d", got)
	}

	childActive = false
	h.checkYesloopSkillCheck() // catch-up: -> REMIND + relay
	time.Sleep(20 * time.Millisecond)
	if !hasSkillCheckState("sc-blocked", yesloopSkillCheckStateRemind) {
		t.Errorf("after subagent returns, threshold crossing should fire, got state %d", stateOfSkillCheck("sc-blocked"))
	}
	if got := atomic.LoadInt64(relays); got != 1 {
		t.Errorf("expected exactly 1 catch-up relay, got %d", got)
	}
}

// TestSkillCheck_NoRelayWithinInterval reproduces the relay flood: once in
// REMIND, ticks inside one 5-min refire interval must not relay. Expected
// traffic is exactly 1 relay (the TRACKING->REMIND transition).
func TestSkillCheck_NoRelayWithinInterval(t *testing.T) {
	resetYesloopSkillCheckState()
	h, s := mustHandler(t)
	dir := shortSockDir(t)
	sockPath := filepath.Join(dir, "sc-flood.sock")
	relays := startInjectSocketListener(t, sockPath+".inject")

	makeSkillCheckAgent(t, h, s, "sc-flood", "sess-sc-flood", "")
	if err := s.AgentUpdate("sc-flood", map[string]any{"sock_path": sockPath}); err != nil {
		t.Fatalf("AgentUpdate sock_path: %v", err)
	}

	h.checkYesloopSkillCheck() // TRACKING, baseline 0
	jumpSkillCheckCounters(t, s, "sc-flood", skillCheckOutputTokenThreshold, 0)
	h.checkYesloopSkillCheck() // -> REMIND, transition relay #1

	// Many heartbeat ticks, all within one refire interval.
	for i := 0; i < 9; i++ {
		h.checkYesloopSkillCheck()
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the listener drain

	if got := atomic.LoadInt64(relays); got != 1 {
		t.Errorf("expected exactly 1 relay (transition) within one refire interval, got %d", got)
	}
	if !hasSkillCheckState("sc-flood", yesloopSkillCheckStateRemind) {
		t.Errorf("agent without marker should remain in REMIND, got state %d", stateOfSkillCheck("sc-flood"))
	}
}

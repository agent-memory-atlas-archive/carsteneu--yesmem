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

// los07dWaitForRelayCount waits until the accept counter reached want, or fails
// after 5s. The relay dial itself is synchronous, but the counter is bumped by
// the listener goroutine, so a fixed sleep races the scheduler: under load the
// assertion read 0 although the connection had been made (flake class #102238).
func los07dWaitForRelayCount(t *testing.T, relays *int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := atomic.LoadInt64(relays); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay count = %d after 5s, want %d", atomic.LoadInt64(relays), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// los07dSettleRelays gives a relay that must NOT have been sent a chance to be
// counted, so a negative assertion cannot pass merely because the listener
// goroutine has not run yet.
func los07dSettleRelays() {
	time.Sleep(50 * time.Millisecond)
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
	h.dataDir = dir
	sockPath := filepath.Join(dir, "sc-freeze.sock")
	relays := startInjectSocketListener(t, sockPath+".inject")

	makeSkillCheckAgent(t, h, s, "sc-freeze", "sess-sc-freeze", "")

	h.checkYesloopSkillCheck() // TRACKING, baseline 0
	jumpSkillCheckCounters(t, s, "sc-freeze", skillCheckOutputTokenThreshold, 0)
	h.checkYesloopSkillCheck() // -> REMIND, transition relay #1
	if !hasSkillCheckState("sc-freeze", yesloopSkillCheckStateRemind) {
		t.Fatalf("precondition: agent should be in REMIND")
	}
	los07dWaitForRelayCount(t, relays, 1)
	los07dSettleRelays()
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
	los07dSettleRelays()
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
	h.dataDir = dir
	sockPath := filepath.Join(dir, "sc-blocked.sock")
	relays := startInjectSocketListener(t, sockPath+".inject")

	makeSkillCheckAgent(t, h, s, "sc-blocked", "sess-sc-blocked", "")

	childActive = true
	h.checkYesloopSkillCheck() // TRACKING, baseline 0
	jumpSkillCheckCounters(t, s, "sc-blocked", skillCheckOutputTokenThreshold, 0)
	h.checkYesloopSkillCheck() // threshold crossed, but frozen
	if !hasSkillCheckState("sc-blocked", yesloopSkillCheckStateTracking) {
		t.Errorf("subagent-active must block transition to REMIND, got state %d", stateOfSkillCheck("sc-blocked"))
	}
	los07dSettleRelays()
	if got := atomic.LoadInt64(relays); got != 0 {
		t.Errorf("subagent-active must not relay, got %d", got)
	}

	childActive = false
	h.checkYesloopSkillCheck() // catch-up: -> REMIND + relay
	los07dWaitForRelayCount(t, relays, 1)
	los07dSettleRelays()
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
	h.dataDir = dir
	sockPath := filepath.Join(dir, "sc-flood.sock")
	relays := startInjectSocketListener(t, sockPath+".inject")

	makeSkillCheckAgent(t, h, s, "sc-flood", "sess-sc-flood", "")

	h.checkYesloopSkillCheck() // TRACKING, baseline 0
	jumpSkillCheckCounters(t, s, "sc-flood", skillCheckOutputTokenThreshold, 0)
	h.checkYesloopSkillCheck() // -> REMIND, transition relay #1

	// Many heartbeat ticks, all within one refire interval.
	for i := 0; i < 9; i++ {
		h.checkYesloopSkillCheck()
		time.Sleep(20 * time.Millisecond)
	}
	los07dWaitForRelayCount(t, relays, 1)
	los07dSettleRelays()

	if got := atomic.LoadInt64(relays); got != 1 {
		t.Errorf("expected exactly 1 relay (transition) within one refire interval, got %d", got)
	}
	if !hasSkillCheckState("sc-flood", yesloopSkillCheckStateRemind) {
		t.Errorf("agent without marker should remain in REMIND, got state %d", stateOfSkillCheck("sc-flood"))
	}
}

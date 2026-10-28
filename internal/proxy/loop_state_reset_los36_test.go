package proxy

import "testing"

// los36LoopMessages holds one identical two-step cycle, which triggers
// LoopIdenticalCycle.
func los36LoopMessages() []any {
	return buildMessages(
		userMsg("fix it"),
		assistantWithToolUse("Edit", "/src/main.go"), toolResult(false, "edited"),
		assistantWithToolUse("Edit", "/src/main.go"), toolResult(false, "edited"),
		assistantWithToolUse("Edit", "/src/main.go"), toolResult(false, "edited"),
		assistantWithToolUse("Edit", "/src/main.go"), toolResult(false, "edited"),
	)
}

// YM-132: WarningCount is per-thread and was never reset. After one warning
// every later loop started at level 2, and after two at level 3 ("stop and ask
// the user") even if the session had long left the first loop behind. A quiet
// phase must clear the count so the next loop starts at level 1 again.
func TestCheckLoopAndFormat_WarningCountResetsWhenLoopEnds(t *testing.T) {
	looping := los36LoopMessages()
	state := &LoopState{}

	warning, level := CheckLoopAndFormat(looping, state)
	if warning == "" || level != 1 {
		t.Fatalf("first loop must warn at level 1, got warning=%q level=%d", warning, level)
	}

	// Drain the cooldown before the quiet phase is evaluated.
	for i := 0; i < loopCooldownRequests; i++ {
		state.Tick()
	}

	quiet := buildMessages(userMsg("hello"), assistantMsg("hi"))
	warning, level = CheckLoopAndFormat(quiet, state)
	if warning != "" || level != 0 {
		t.Fatalf("a quiet phase must not warn, got warning=%q level=%d", warning, level)
	}
	// One quiet check is not a quiet phase: the signal can flicker on the
	// window boundary, and the ladder must survive that.
	if state.WarningCount == 0 {
		t.Fatal("a single signal-free check must not reset the warning count")
	}
	CheckLoopAndFormat(quiet, state)
	if state.WarningCount != 0 {
		t.Fatalf("WarningCount must reset after %d quiet checks, got %d", loopWarningResetAfter, state.WarningCount)
	}

	// The next loop is a new loop and starts at level 1 again.
	warning, level = CheckLoopAndFormat(looping, state)
	if warning == "" || level != 1 {
		t.Errorf("a new loop after a quiet phase must start at level 1, got warning=%q level=%d", warning, level)
	}
}

// The escalation must survive a signal that flickers between sightings: an
// edit/test loop whose pattern sits on the window boundary delivers a signal
// only every other check, and a single miss must not send it back to level 1.
func TestCheckLoopAndFormat_FlickeringSignalKeepsEscalating(t *testing.T) {
	looping := los36LoopMessages()
	quiet := buildMessages(userMsg("hello"), assistantMsg("hi"))
	state := &LoopState{}

	drain := func() {
		for j := 0; j < loopCooldownRequests; j++ {
			state.Tick()
		}
	}

	_, l1 := CheckLoopAndFormat(looping, state)
	drain()
	CheckLoopAndFormat(quiet, state) // one miss between two sightings
	_, l2 := CheckLoopAndFormat(looping, state)
	drain()
	CheckLoopAndFormat(quiet, state)
	_, l3 := CheckLoopAndFormat(looping, state)

	if l1 != 1 || l2 != 2 || l3 != 3 {
		t.Errorf("the ladder must keep climbing through flicker, got %d, %d, %d", l1, l2, l3)
	}
}

// The escalation within a running loop must still work: a second detection
// while the loop persists reaches level 2.
func TestCheckLoopAndFormat_StillEscalatesWithinTheSameLoop(t *testing.T) {
	looping := los36LoopMessages()
	state := &LoopState{}

	if _, level := CheckLoopAndFormat(looping, state); level != 1 {
		t.Fatalf("first detection must be level 1, got %d", level)
	}
	for i := 0; i < loopCooldownRequests; i++ {
		state.Tick()
	}
	if _, level := CheckLoopAndFormat(looping, state); level != 2 {
		t.Errorf("a persisting loop must escalate to level 2, got %d", level)
	}
}

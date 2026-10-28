package daemon

import (
	"strings"
	"testing"
	"time"
)

// los38rFreezeLogLine is the text every guard writes when it skips a tick
// because a task() subagent is running.
const los38rFreezeLogLine = "frozen: a task() subagent is active"

// YM-344 (2): the idle guard reports the subagent freeze on a clock, the way the
// stagnation, DONE-guard and done-verify layers already do. What the flag got
// wrong is the mark's lifetime, not the interval: a tick that returns at one of
// the layer's early exits never reaches the line that clears it, so the mark
// outlived the episode and every later episode went unreported.
func TestLos38R_IdleFreezeReportsAgainOnTheNextEpisode(t *testing.T) {
	resetIdleState()
	h, s := mustHandler(t)
	stubOpencodeChildActive(t, true)

	makeYesloopAgent(t, h, s, "idle38r", "sess-idle38r", testPID, false, time.Hour)
	agent, err := s.AgentGet("idle38r")
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}

	buf := captureLog(t)

	// First frozen tick: the guard records the freeze and reports it.
	h.checkOneAgent(*agent, nil)
	if got := strings.Count(buf.String(), los38rFreezeLogLine); got != 1 {
		t.Fatalf("first freeze reported %d times, want 1:\n%s", got, buf.String())
	}

	// A repeat tick inside the report window stays silent: the gate is a clock,
	// not "log every frozen tick". Without this the test would pass for an
	// implementation without any gate at all.
	h.checkOneAgent(*agent, nil)
	if got := strings.Count(buf.String(), los38rFreezeLogLine); got != 1 {
		t.Fatalf("a second frozen tick inside the window reported again (%d total):\n%s", got, buf.String())
	}

	// A tick that leaves the layer's own paths must not clear the mark — it is
	// the reason the mark has to age on a clock instead of being a flag.
	paused := *agent
	paused.Status = "paused"
	h.checkOneAgent(paused, nil)

	yesloopIdleAgentsMu.Lock()
	st := yesloopIdleAgents["idle38r"]
	if st == nil {
		yesloopIdleAgentsMu.Unlock()
		t.Fatal("the guard holds no state for the agent")
	}
	mark := st.subagentFrozenAt
	if mark.IsZero() {
		yesloopIdleAgentsMu.Unlock()
		t.Fatal("the freeze mark must carry the time of its report")
	}
	// Age it the way a long episode would: the next report is owed once the last
	// one is older than subagentFreezeLogEvery.
	st.subagentFrozenAt = time.Now().Add(-(subagentFreezeLogEvery + time.Minute))
	yesloopIdleAgentsMu.Unlock()

	h.checkOneAgent(*agent, nil)

	if got := strings.Count(buf.String(), los38rFreezeLogLine); got != 2 {
		t.Errorf("the second freeze episode reported %d times in total, want 2:\n%s", got, buf.String())
	}
	if mark.After(time.Now()) {
		t.Errorf("the freeze mark is in the future: %s", mark)
	}
}

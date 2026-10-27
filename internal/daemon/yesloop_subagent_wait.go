package daemon

import (
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// subagentFreezeLogEvery bounds how often the freeze of one agent is reported.
// A frozen guard that never reports itself is invisible, and the reason the
// freeze exists (#89428) was found through the log line that named it. A plain
// once-per-episode flag would go silent for a second episode that starts after
// the agent left the layer's own paths (paused, stopped, stream inactive), so
// the report repeats on a clock instead.
const subagentFreezeLogEvery = 30 * time.Minute

// agentWaitingInSubagent reports whether the agent's current turn is blocked
// inside a task() subagent. A relay written into an agent in that state only
// queues as a pending user message and hits the agent after the subagent
// returns (#89428), where it interrupts the wrong turn.
//
// The idle guard, the skillcheck and the broken-toolcall guard already gated on
// this state; stagnation, done-verify and the DONE-guard did not, and the last
// two could even pause an agent in the middle of the review its child was
// running.
//
// Two signals, mirroring agentInSubagent: an active child session in opencode's
// own DB (the accurate source for a backend with a session API), and the stream
// marker the proxy leaves on the parent's thread for a backend without one
// (YM-253). The marker is bounded — a crash right after the child cannot hold
// every guard off for good. An opencode agent whose session id is not bound yet
// has neither signal, so the callers bind on demand before asking.
func (h *Handler) agentWaitingInSubagent(agent storage.Agent) bool {
	var fields map[string]any
	if agent.SessionID != "" {
		fields = h.getStreamFields(agent.SessionID)
	}
	return h.agentWaitingInSubagentWith(agent, fields)
}

// agentWaitingInSubagentWith is agentWaitingInSubagent for a caller that has
// already read the agent's stream fields (the stagnation sweep reads them for
// its stream_active gate anyway).
func (h *Handler) agentWaitingInSubagentWith(agent storage.Agent, fields map[string]any) bool {
	return h.agentInSubagent(agent.OpencodeSessionID, fields)
}

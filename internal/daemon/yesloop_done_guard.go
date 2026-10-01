package daemon

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// --- State machine for yesloop DONE-guard refire logic ---
//
// Layer 4 of the yesloop guarantee. When a yesloop agent has all 6 phase
// headers in its scratchpad but ValidatePhaseBlocks reports field errors,
// the guard does NOT immediately pause. Instead it relays the concrete
// missing field to the agent and re-fires up to doneGuardMaxRefires times
// at doneGuardRefireInterval intervals. Only after the max is exceeded is
// the agent paused. This gives cooperative agents a chance to fix cosmetic
// format errors (e.g. **Build:** instead of **Tests run:**) without being
// killed. State is in-memory and lost on daemon restart — acceptable per
// spec, mirrors yesloop_idle.go.

const (
	doneGuardStateRefiring = iota
	doneGuardStatePaused
)

const (
	doneGuardMaxRefires     = 2
	doneGuardRefireInterval = 90 * time.Second
)

// doneGuardState tracks the refire state for a single agent.
type doneGuardState struct {
	state       int
	refireCount int
	lastRelayAt time.Time
}

var (
	yesloopDoneGuardAgents   = make(map[string]*doneGuardState)
	yesloopDoneGuardAgentsMu sync.Mutex
)

// resetDoneGuardState clears the done-guard state map. Used in tests.
func resetDoneGuardState() {
	yesloopDoneGuardAgentsMu.Lock()
	yesloopDoneGuardAgents = make(map[string]*doneGuardState)
	yesloopDoneGuardAgentsMu.Unlock()
}

// checkOneDoneGuardAgent is the per-agent refire state machine. Called from
// heartbeat.checkYesloopDoneGuard with the agent's current validation result.
// The separate signature lets tests drive the machine without going through
// the AgentList loop.
//
// Lock discipline: the state map mutex is held for map reads/writes only.
// It is released around pauseAgent/notifyOrchestrator/sendDoneGuardRelay
// (which may block on DB or socket I/O) to avoid blocking concurrent agents.
func (h *Handler) checkOneDoneGuardAgent(agent storage.Agent, result ValidationResult) {
	// Phase 5 evidence verification: when the format is compliant, verify the
	// reported review subagent session ids against opencode's own session DB.
	// Agents without a captured opencode session id skip verification. A
	// failure downgrades to a non-compliant result that feeds the same
	// refire/pause machine as a format failure, naming the invalid field.
	if result.Compliant && agent.OpencodeSessionID != "" && len(result.Phase5Evidence) > 0 {
		if evErrs := VerifySubagentEvidence(h.ocDBPath, agent.OpencodeSessionID, result.Phase5Evidence); len(evErrs) > 0 {
			result = ValidationResult{
				Compliant:      false,
				PhaseCount:     6,
				FieldErrors:    evErrs,
				Phase5Evidence: result.Phase5Evidence,
			}
		}
	}

	yesloopDoneGuardAgentsMu.Lock()

	state, exists := yesloopDoneGuardAgents[agent.ID]

	// Agent fixed the format issue → clear state and stop tracking.
	// Additionally, if the agent was paused by DONE-GUARD but the scratchpad
	// became compliant post-pause (e.g. agent continued working, daemon
	// restart lost in-memory state), unpause it so the status reflects reality.
	if result.Compliant {
		if exists {
			log.Printf("[done-guard] agent %s (%s) now compliant — clearing refire state",
				agent.ID, agent.Section)
			delete(yesloopDoneGuardAgents, agent.ID)
		}
		if agent.Status == "paused" && strings.HasPrefix(agent.Progress, "paused: DONE-GUARD:") {
			log.Printf("[done-guard] agent %s (%s) RECOVERED: scratchpad now compliant — unpausing (was: %s)",
				agent.ID, agent.Section, agent.Progress)
			yesloopDoneGuardAgentsMu.Unlock()
			h.unpauseAgent(agent.ID, "DONE-GUARD", fmt.Sprintf("scratchpad now compliant (was: %s)", agent.Progress))
			h.notifyOrchestrator(agent, fmt.Sprintf(
				"RECOVERED: agent %s (%s) unpaused by DONE-GUARD — scratchpad now compliant (was: %s)",
				agent.ID, agent.Section, agent.Progress))
			return
		}
		yesloopDoneGuardAgentsMu.Unlock()
		return
	}

	// First detection: register state and send initial relay (refireCount=0).
	if !exists {
		state = &doneGuardState{
			state:       doneGuardStateRefiring,
			lastRelayAt: time.Now(),
		}
		yesloopDoneGuardAgents[agent.ID] = state
		log.Printf("[done-guard] agent %s (%s) validation failed — sending initial relay (errors: %s)",
			agent.ID, agent.Section, summarizeErrors(result))
		yesloopDoneGuardAgentsMu.Unlock()
		h.sendDoneGuardRelay(agent, result)
		return
	}

	if state.state != doneGuardStateRefiring {
		yesloopDoneGuardAgentsMu.Unlock()
		return
	}

	// Interval-gated: not enough time since last relay.
	if time.Since(state.lastRelayAt) < doneGuardRefireInterval {
		yesloopDoneGuardAgentsMu.Unlock()
		return
	}

	// Interval elapsed — increment refire count.
	state.refireCount++
	if state.refireCount > doneGuardMaxRefires {
		// Pause. Terminal state for this agent's tracking.
		state.state = doneGuardStatePaused
		reason := fmt.Sprintf("DONE-GUARD: phase validation failed — %s", summarizeErrors(result))
		log.Printf("[done-guard] agent %s (%s) ESCALATION: pausing after %d refires (errors: %s)",
			agent.ID, agent.Section, state.refireCount, summarizeErrors(result))
		yesloopDoneGuardAgentsMu.Unlock()
		h.pauseAgent(agent.ID, reason)
		h.notifyOrchestrator(agent, fmt.Sprintf(
			"DEAD_AGENT: Agent %s (%s) paused by DONE-GUARD after %d relay attempts — phase validation failed: %s%s",
			agent.ID, agent.Section, state.refireCount, summarizeErrors(result), orchestratorPauseHint))
		return
	}

	// Refire: send relay again, update timestamp.
	log.Printf("[done-guard] agent %s (%s) refire %d/%d (errors: %s)",
		agent.ID, agent.Section, state.refireCount, doneGuardMaxRefires, summarizeErrors(result))
	state.lastRelayAt = time.Now()
	yesloopDoneGuardAgentsMu.Unlock()
	h.sendDoneGuardRelay(agent, result)
}

// buildDoneGuardRelayMessage constructs the relay body sent to the agent.
// Pure function — testable without socket I/O. The message is metachar-free
// (no markdown, no backticks) because it is written as a single line to the
// agent's PTY inject socket.
func buildDoneGuardRelayMessage(result ValidationResult) string {
	return fmt.Sprintf(
		"DONE-GUARD: phase validation failed. Missing or invalid fields: %s. "+
			"Fix the listed fields in your scratchpad phase blocks and call update_agent_status when done.",
		summarizeErrors(result))
}

// sendDoneGuardRelay sends the validation-failure relay to a yesloop agent
// via the PTY inject socket. Mirrors sendDoneVerifyRelay: callers set
// state.lastRelayAt BEFORE calling (or rely on the initial-relay path) so a
// failed dial still counts as a relay attempt for refire-gating purposes
// (prevents rapid escalation when sockets are flaky).
func (h *Handler) sendDoneGuardRelay(agent storage.Agent, result ValidationResult) {
	if agent.SockPath == "" {
		log.Printf("[done-guard] relay to agent %s skipped: no sock_path", agent.ID)
		return
	}
	injectPath := agent.SockPath + ".inject"
	wrapped := fmt.Sprintf("[RELAY from=done-guard] %s", buildDoneGuardRelayMessage(result))

	conn, err := net.DialTimeout("unix", injectPath, 3*time.Second)
	if err != nil {
		log.Printf("[done-guard] relay to agent %s failed: %v", agent.ID, err)
		return
	}
	defer conn.Close()
	conn.Write([]byte(wrapped + "\r\n"))
}

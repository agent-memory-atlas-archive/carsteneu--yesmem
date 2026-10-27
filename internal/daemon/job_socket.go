package daemon

import (
	"fmt"
)

// jobBroadcastSender is the sender every sandboxed run announces under.
const jobBroadcastSender = "sandboxed-job"

// jobSocketAllowed is the RPC surface a sandboxed run may reach (YM-267).
//
// A sandbox constrains the process the daemon started, not the daemon itself. A
// job that could reach the full socket would simply ask for what the sandbox
// denies: spawn_agent starts an unsandboxed binary, execute_cap runs a cap
// declared sandbox:none on the host, schedule creates exactly such a job. So the
// job socket is default-deny and carries only the calls a run needs to do its own
// work: its cap store, the LLM bridge, memory reads and writes, and the ability
// to announce a result.
//
// Two groups that look harmless stay out because they hand the run the daemon's
// own privileges instead of the run's:
//   - the code-navigation RPCs. They take project_dir / _cwd from the caller and
//     read files relative to it, so they are a general read primitive through the
//     daemon, masked secret paths included (security review YM-267). A run
//     already has the filesystem the sandbox gives it; it does not need ours.
//   - the scratchpad RPCs. project and owner are caller-chosen: the write path
//     creates <dataDir>/projects/<project>/assets and can overwrite any agent's
//     section, which is exactly the steering the send_to exclusion is about. The
//     daemon writes a run's result section, not the run itself.
//
// Beyond those, four classes stay out on purpose, and the decision belongs to the
// allowance rather than to each method:
//   - anything that starts a process or ends one (spawn_agent, resume_agent,
//     open_agent_terminal, stop_agent, execute_cap, schedule, relay_agent),
//     because that is the boundary itself;
//   - cap authoring (save_cap, register_caps, remember with category "cap"),
//     because a cap row is code the scheduler will run;
//   - configuration and identity (set_config, set_persona, pin, set_proxy_state);
//   - steering other work: send_to, and the task/plan lifecycle (resolve,
//     resolve_by_text, reopen, set_plan, update_plan, complete_plan). Closing
//     tasks or writing plans belongs to the caller that supervises the run.
//
// Anything not listed is refused with an error naming the method, so a missing
// entry surfaces as a visible failure rather than as a silent widening.
var jobSocketAllowed = map[string]bool{
	// Cap plumbing the bash and REPL polyfills call.
	"cap_store":    true,
	"llm_complete": true,
	"ping":         true,

	// Identity and bookkeeping hooks/CLI helpers use.
	"whoami":            true,
	"idle_tick":         true,
	"register_pid":      true,
	"track_session_end": true,

	// Memory.
	"search":              true,
	"hybrid_search":       true,
	"deep_search":         true,
	"get_learnings":       true,
	"remember":            true,
	"docs_search":         true,
	"get_compacted_stubs": true,
	"expand_context":      true,

	// One-to-many announcement. send_to (direct delivery to a chosen agent) stays
	// out: it would let a job steer an unsandboxed agent.
	"broadcast": true,
}

// handleJobSocket serves the restricted socket a sandboxed run mounts. It is the
// daemon-side half of that boundary: the mount decides which socket a job can
// open, this decides what the socket answers.
func (h *Handler) handleJobSocket(req Request) Response {
	if !jobSocketAllowed[req.Method] {
		return errorResponse(fmt.Sprintf("method %q is not available to sandboxed jobs", req.Method))
	}
	// A cap row is executable code: the scheduler runs a cap's script body, and the
	// profile that script declares overrides the job's, sandbox:none included
	// (handler_scheduler.go los35cResolveBashCap). remember accepts every valid
	// category, "cap" among them, and stores the context verbatim — so without this
	// guard a job could plant or supersede a cap and have the next run execute it
	// on the host (YM-267, cold review).
	if req.Method == "remember" {
		if category, _ := req.Params["category"].(string); category == "cap" {
			return errorResponse("remember: category \"cap\" is not available to sandboxed jobs")
		}
		// The trust gate is what keeps a job from retiring a cap row through
		// supersedes; the override removes that gate, so a job may not use it.
		if force, _ := req.Params["force_supersede"].(bool); force {
			return errorResponse("remember: force_supersede is not available to sandboxed jobs")
		}
		// The category guard above looks at the row being written. The row being
		// retired decides just as much: a cap row it replaces stops being run.
		if target, _ := req.Params["supersedes"].(float64); target > 0 {
			if l, err := h.store.GetLearning(int64(target)); err == nil && l.Category == "cap" {
				return errorResponse("remember: superseding a cap row is not available to sandboxed jobs")
			}
		}
	}
	// The sender is the caller's claim and the channel routes on it: a run that
	// could name itself "yesmem" or after another agent could pass its message off
	// as theirs. Its announcement is labelled as what it is. The mark is a private
	// type: only this function can set it, and no JSON payload decodes into it, so
	// a socket client cannot borrow the job's permission to name its own sender
	// (YM-271).
	if req.Method == "broadcast" {
		req.Params["sender"] = jobBroadcastSender
		req.Params["_job_caller"] = jobCallerMark{}
	}
	return h.Handle(req)
}

// jobCallerMark is the in-process proof that a request came from the sandboxed
// job socket. A private empty struct cannot be produced by json.Unmarshal, which
// is what makes it usable as a marker: an unexported Go type is not a value any
// client can send.
type jobCallerMark struct{}

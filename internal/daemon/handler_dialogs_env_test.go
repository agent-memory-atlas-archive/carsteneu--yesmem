package daemon

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// --- resolveSessionID: env-derived opencode ids ---
//
// Under opencode v2 the `yesmem mcp` children belong to the shared
// `opencode serve --service` process, so YESMEM_SESSION_ID in their environment
// names the session created last — never the caller. The MCP layer marks such
// an id with _session_id_source="env", and the daemon then prefers the caller's
// working directory.

func TestResolveSessionID_EnvDerivedOpencodeSessionLosesToCallerCWD(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	const worktree = "/worktrees/env-loses"
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-env-loses", Project: "/repo/env", Section: "env-sec",
		SessionID: "cwd-sess", Status: "running", Backend: "opencode",
		OpencodeSessionID: "ses_cwd",
	})
	seedRootSession(t, worktree, "ses_cwd")

	got := h.resolveSessionID(map[string]any{
		"_session_id":        "opencode:ses_LEAKED",
		"_session_id_source": "env",
		"_source_agent":      "opencode",
		"_cwd":               worktree,
		"_caller_pid":        float64(opencodeSharedServicePID),
	}, "missing")
	if got != "cwd-sess" {
		t.Fatalf("got %q, want cwd-sess from the caller's directory, not the leaked env id", got)
	}
}

// The v1 layout stays intact: there the MCP child is the direct child of the
// agent the daemon spawned, so the caller PID belongs to a running agent row
// and the env id is the caller's own session.
func TestResolveSessionID_EnvDerivedOpencodeSessionHonouredForAgentPID(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-v1", Project: "/repo/v1", Section: "v1-sec",
		SessionID: "v1-own-sess", PID: 4242, Status: "running", Backend: "opencode",
	})

	got := h.resolveSessionID(map[string]any{
		"_session_id":        "opencode:ses_v1_env",
		"_session_id_source": "env",
		"_source_agent":      "opencode",
		"_caller_pid":        float64(4242),
	}, "missing")
	if got != "opencode:ses_v1_env" {
		t.Fatalf("got %q, want the env id honoured for a daemon-spawned agent's PID", got)
	}
}

// A Claude session has no per-call injection and keeps relying on the env id.
func TestResolveSessionID_EnvDerivedClaudeSessionStaysAuthoritative(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)

	got := h.resolveSessionID(map[string]any{
		"_session_id":        "claude-sid",
		"_session_id_source": "env",
		"_caller_pid":        float64(opencodeSharedServicePID),
	}, "missing")
	if got != "claude-sid" {
		t.Fatalf("got %q, want claude-sid", got)
	}
}

// An unmarked _session_id is a per-call injection and keeps precedence, even
// when the caller PID is the shared service PID.
func TestResolveSessionID_InjectedSessionKeepsPrecedence(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)

	got := h.resolveSessionID(map[string]any{
		"_session_id":   "opencode:ses_injected",
		"_source_agent": "opencode",
		"_cwd":          "/worktrees/never-matched",
		"_caller_pid":   float64(opencodeSharedServicePID),
	}, "missing")
	if got != "opencode:ses_injected" {
		t.Fatalf("got %q, want the injected per-call id", got)
	}
}

// Production state, not the empty fixture: the plugin registers the shared
// service PID on every session.created, so the map (and the PID file) hold a
// session for that PID. The cwd path must still win — resolving through the
// registration would hand the call to whichever session registered last, which
// is the misattribution this resolver exists to avoid.
func TestResolveSessionID_RegisteredServicePIDDoesNotOutrankCallerCWD(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	const worktree = "/worktrees/registered-service-pid"
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-registered", Project: "/repo/reg", Section: "reg-sec",
		SessionID: "cwd-wins-sess", Status: "running", Backend: "opencode",
		OpencodeSessionID: "ses_reg",
	})
	seedRootSession(t, worktree, "ses_reg")
	// The plugin's registration, in the order production produces it.
	if resp := h.handleRegisterPID(map[string]any{
		"session_id": "opencode:ses_LASTREG", "pid": float64(opencodeSharedServicePID), "source_agent": "opencode",
	}); resp.Error != "" {
		t.Fatalf("register pid: %s", resp.Error)
	}

	got := h.resolveSessionID(map[string]any{
		"_session_id":        "opencode:ses_ENVLEAK",
		"_session_id_source": "env",
		"_source_agent":      "opencode",
		"_cwd":               worktree,
		"_caller_pid":        float64(opencodeSharedServicePID),
	}, "missing")
	if got != "cwd-wins-sess" {
		t.Fatalf("got %q, want cwd-wins-sess (the registration stands for the last session, not this caller)", got)
	}
}

// A PID that belongs to a running agent keeps the PID path: that is the v1
// layout, where the MCP child is the agent's direct child.
func TestResolveSessionID_AgentPIDStillResolvesThroughThePIDPath(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	newStoredAgent(t, h, storage.Agent{
		ID: "agent-v1-pid", Project: "/repo/v1pid", Section: "v1pid-sec",
		SessionID: "v1-pid-sess", PID: 4243, Status: "running", Backend: "opencode",
	})
	h.pidMapMu.Lock()
	h.pidMap["opencode:ses_v1_registered"] = 4243
	h.pidMapMu.Unlock()

	got := h.resolveSessionID(map[string]any{
		"_source_agent": "opencode",
		"_caller_pid":   float64(4243),
	}, "missing")
	if got != "opencode:ses_v1_registered" {
		t.Fatalf("got %q, want the registered session of a daemon-spawned agent's PID", got)
	}
}

// Claude sessions have no shared-service layout: their PID path stays intact.
func TestResolveSessionID_ClaudePIDPathUnchanged(t *testing.T) {
	h, _ := mustHandler(t)
	h.pidMapMu.Lock()
	h.pidMap["claude-sess"] = 4244
	h.pidMapMu.Unlock()

	got := h.resolveSessionID(map[string]any{"_caller_pid": float64(4244)}, "missing")
	if got != "claude-sess" {
		t.Fatalf("got %q, want claude-sess", got)
	}
}

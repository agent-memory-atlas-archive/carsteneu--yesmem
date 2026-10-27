package daemon

import "testing"

// The binding overrideCallerSession resolves is what the set_config and get_config
// guards rest on. These are unit tests of that resolution, next to the behavioural
// tests in los59b_proxy_state_scope_test.go.

// A bound caller is the ordinary case, and the ordinary case is what the guard is
// for: a loop agent that names another session is refused, because its own agent
// row in the verified working directory binds it to its own session.
func TestLos59bOverrideCallerSessionIgnoresAnOpencodeIdFromThePeerChain(t *testing.T) {
	h, _ := mustHandler(t)
	los59bWithProcess(t, 4242, "/home/u/.local/bin/yesmem\x00mcp")

	// The plugin registers the shared service pid once per session, so the map ends
	// up holding whichever opencode session was created last machine-wide. That is a
	// guess, not a binding.
	h.pidMapMu.Lock()
	h.pidMap["opencode:ses_last_created"] = 4242
	h.pidMapMu.Unlock()
	if got := h.overrideCallerSession(map[string]any{"_peer_pid": 4242}); got != "" {
		t.Errorf("an opencode id read from the shared service pid counted as a binding: %q", got)
	}

	// A Claude Code session registers its own process in that map, so the same entry
	// is a binding there.
	h.pidMapMu.Lock()
	delete(h.pidMap, "opencode:ses_last_created")
	h.pidMap["claude:0f0e0d0c"] = 4242
	h.pidMapMu.Unlock()
	if got := h.overrideCallerSession(map[string]any{"_peer_pid": 4242}); got != "claude:0f0e0d0c" {
		t.Errorf("a Claude session registered for its own pid did not bind: %q", got)
	}
}

// The environment names the session for the harnesses that set it; the names are
// the ones the MCP layer reads, or the two sides disagree about what a process'
// session is called.
func TestLos59bPeerEnvSessionReadsTheNamesTheMCPLayerReads(t *testing.T) {
	for _, name := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "YESMEM_SESSION_ID"} {
		los59bWithProcess(t, 4242, "", name+"=opencode:ses_env")
		if got := peerEnvSession(4242); got != "opencode:ses_env" {
			t.Errorf("%s: peerEnvSession = %q, want the session from the environment", name, got)
		}
	}
}

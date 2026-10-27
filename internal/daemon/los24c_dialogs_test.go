package daemon

import "testing"

// active_session_opencode can hold a codex channel since register_pid writes it
// for codex too (audit 16.10.2026, YM-328). The last resort of
// resolveSessionID handed that value to every caller, so a Claude process with
// no identity of its own would be attributed to a foreign codex session. A guess
// may be empty, but it must not name a session the caller cannot be held to.
func TestResolveActiveSessionFallback_DoesNotReturnCodexChannelToClaudeCaller(t *testing.T) {
	h, store := mustHandler(t)
	if err := store.SetProxyState("active_session_opencode", "codex:3fa85f64-5717-4562-b3fc-2c963f66afa6"); err != nil {
		t.Fatalf("seed proxy state: %v", err)
	}

	if got := h.resolveActiveSessionFallback(map[string]any{}); got != "" {
		t.Errorf("fallback for an unidentified caller = %q, want empty", got)
	}
}

// The opencode branch is the reason the key is persisted at all: an opencode
// caller must still resolve its session from it after a restart.
func TestResolveActiveSessionFallback_OpencodeCallerStillResolves(t *testing.T) {
	h, store := mustHandler(t)
	if err := store.SetProxyState("active_session_opencode", "opencode:ses-42"); err != nil {
		t.Fatalf("seed proxy state: %v", err)
	}

	got := h.resolveActiveSessionFallback(map[string]any{"_source_agent": "opencode"})
	if got != "opencode:ses-42" {
		t.Errorf("opencode fallback = %q, want opencode:ses-42", got)
	}
}

// _source_agent is caller-supplied, and the key is shared with codex, so a
// caller that labels itself opencode must still not receive a codex channel.
func TestResolveActiveSessionFallback_OpencodeLabelDoesNotUnlockACodexChannel(t *testing.T) {
	h, store := mustHandler(t)
	if err := store.SetProxyState("active_session_opencode", "codex:3fa85f64-5717-4562-b3fc-2c963f66afa6"); err != nil {
		t.Fatalf("seed proxy state: %v", err)
	}

	if got := h.resolveActiveSessionFallback(map[string]any{"_source_agent": "opencode"}); got != "" {
		t.Errorf("fallback = %q, want empty for a value outside the opencode namespace", got)
	}
}

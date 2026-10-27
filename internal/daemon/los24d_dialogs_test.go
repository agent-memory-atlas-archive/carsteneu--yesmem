package daemon

import (
	"testing"
)

// YM-338 (Los 24d): register_pid wrote active_session_opencode for codex too,
// although the key names the opencode channel. resolveActiveSessionFallback
// reads it for opencode callers, so a codex registration displaced the opencode
// session that fallback is meant to find.
func TestRegisterPIDKeepsTheOpencodeChannelForOpencode(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.SetProxyState("active_session_opencode", "opencode:ses-existing"); err != nil {
		t.Fatalf("seed proxy state: %v", err)
	}

	resp := h.handleRegisterPID(map[string]any{
		"session_id":   "codex:thread-1",
		"pid":          float64(4242),
		"source_agent": "codex",
	})
	if resp.Error != "" {
		t.Fatalf("register_pid(codex): %s", resp.Error)
	}
	got, err := s.GetProxyState("active_session_opencode")
	if err != nil {
		t.Fatalf("read proxy state: %v", err)
	}
	if got != "opencode:ses-existing" {
		t.Errorf("active_session_opencode = %q after a codex registration, want the opencode session untouched", got)
	}

	// The opencode branch must still persist its own session.
	resp = h.handleRegisterPID(map[string]any{
		"session_id":   "opencode:ses-new",
		"pid":          float64(4243),
		"source_agent": "opencode",
	})
	if resp.Error != "" {
		t.Fatalf("register_pid(opencode): %s", resp.Error)
	}
	if got, err = s.GetProxyState("active_session_opencode"); err != nil || got != "opencode:ses-new" {
		t.Errorf("active_session_opencode = %q (err %v) after an opencode registration, want opencode:ses-new", got, err)
	}
}

// YM-338 (Los 24d): the opencode branch of resolveActiveSessionFallback wrote the
// resolved id into h.activeSessionID, the global last-writer state. A caller that
// merely labelled itself opencode thereby handed its session to every later
// caller without an identity of its own.
func TestResolveActiveSessionFallbackDoesNotLeakIntoTheAnonymousPath(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.SetProxyState("active_session_opencode", "opencode:ses-42"); err != nil {
		t.Fatalf("seed proxy state: %v", err)
	}

	if got := h.resolveActiveSessionFallback(map[string]any{"_source_agent": "opencode"}); got != "opencode:ses-42" {
		t.Fatalf("opencode fallback = %q, want opencode:ses-42", got)
	}
	if got := h.resolveActiveSessionFallback(map[string]any{}); got != "" {
		t.Errorf("unidentified caller after an opencode-labelled call = %q, want empty", got)
	}
}

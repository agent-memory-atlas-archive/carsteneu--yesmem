package daemon

import (
	"testing"
)

// YM-414 [PRX-P57]: set_proxy_state wrote any key for any caller. The daemon
// socket is shared, so a client that is not the proxy could overwrite the
// per-thread state of another session (frozen prefix, decay ages, timestamps,
// eager-stub sets, sawtooth budget, session model) and change what the proxy
// does for that thread. The channel endpoints were closed against the same
// class (YM-204, YM-235): a value the caller sends is not proof of ownership,
// the peer the kernel reports is.

// los58dWithPeer pins the argv peerIsProxy reads for a simulated socket peer.
func los58dWithPeer(t *testing.T, argv string) {
	t.Helper()
	prev := procCmdline
	procCmdline = func(int) string { return argv }
	t.Cleanup(func() { procCmdline = prev })
}

func TestLos58dSetProxyStateRefusesAForeignPeerForAThreadKey(t *testing.T) {
	requireProcTable(t)
	h, s := mustHandler(t)
	los58dWithPeer(t, "/usr/bin/python3\x00-c\x00import os")

	for _, key := range []string{
		"frozen:ses_victim",
		"frozen_markers:ses_victim",
		"decay:ses_victim",
		"timestamps:ses_victim",
		"eagerstub:ses_victim",
		"sawtooth:ses_victim",
		"session_model:ses_victim",
	} {
		resp := h.Handle(Request{Method: "set_proxy_state", Params: map[string]any{
			"key": key, "value": "written-by-a-foreign-client", "_peer_pid": 4242,
		}})
		if resp.Error == "" {
			t.Errorf("%s: a client that is not the proxy wrote another session's state", key)
		}
		if value, _ := s.GetProxyState(key); value != "" {
			t.Errorf("%s: the refused write was stored anyway: %q", key, value)
		}
	}
}

func TestLos58dSetProxyStateAllowsTheProxyForAThreadKey(t *testing.T) {
	h, s := mustHandler(t)
	los58dWithPeer(t, "/home/u/.local/bin/yesmem\x00proxy\x00--port\x004000")

	resp := h.Handle(Request{Method: "set_proxy_state", Params: map[string]any{
		"key": "frozen:ses_live", "value": `{"messages":[]}`, "_peer_pid": 4242,
	}})
	if resp.Error != "" {
		t.Fatalf("the proxy writes per-thread state and must keep doing so: %s", resp.Error)
	}
	if value, _ := s.GetProxyState("frozen:ses_live"); value != `{"messages":[]}` {
		t.Errorf("frozen:ses_live = %q, want the written payload", value)
	}
}

// A call without a peer is the daemon's own code, and the guard abstains there,
// exactly as channelInboxSession does.
func TestLos58dSetProxyStateKeepsInternalCallsWorking(t *testing.T) {
	h, s := mustHandler(t)

	resp := h.Handle(Request{Method: "set_proxy_state", Params: map[string]any{
		"key": "frozen:ses_internal", "value": `{"messages":[]}`,
	}})
	if resp.Error != "" {
		t.Fatalf("an internal write must not need a peer: %s", resp.Error)
	}
	if value, _ := s.GetProxyState("frozen:ses_internal"); value == "" {
		t.Error("the internal write was not stored")
	}
}

// The keys that carry no thread are not the subject of this guard: `yesmem
// config set` mirrors the token-threshold family through this RPC and must keep
// working from a process that is not the proxy.
func TestLos58dSetProxyStateAllowsTheGlobalConfigSlotFromAClient(t *testing.T) {
	h, s := mustHandler(t)
	los58dWithPeer(t, "/usr/local/bin/yesmem\x00config\x00set\x00token_threshold\x00400000")

	resp := h.Handle(Request{Method: "set_proxy_state", Params: map[string]any{
		"key": "config_override:token_threshold:deepseek", "value": "400000", "_peer_pid": 4242,
	}})
	if resp.Error != "" {
		t.Fatalf("a key without a thread subject must stay writable: %s", resp.Error)
	}
	if value, _ := s.GetProxyState("config_override:token_threshold:deepseek"); value != "400000" {
		t.Errorf("config slot = %q, want 400000", value)
	}
}

// source_agent:<session> is per session as well. It routes which agent a session
// belongs to (briefing, learnings attribution), so a foreign client that writes
// it changes another session's attribution.
func TestLos58dSetProxyStateRefusesAForeignPeerForTheSourceAgent(t *testing.T) {
	requireProcTable(t)
	h, s := mustHandler(t)
	los58dWithPeer(t, "/usr/bin/python3\x00-c\x00import os")

	resp := h.Handle(Request{Method: "set_proxy_state", Params: map[string]any{
		"key": "source_agent:opencode:ses_victim", "value": "codex", "_peer_pid": 4242,
	}})
	if resp.Error == "" {
		t.Error("a foreign client rewrote another session's source_agent")
	}
	if value, _ := s.GetProxyState("source_agent:opencode:ses_victim"); value != "" {
		t.Errorf("the refused write was stored: %q", value)
	}
}

// A write guard alone leaves the destructive sibling open: deleting the family
// removes the same state for every thread at once, and the proxy's own cache
// reset is the only legitimate caller.
func TestLos58dDeleteProxyStatePrefixRefusesAForeignPeerForAThreadFamily(t *testing.T) {
	requireProcTable(t)
	h, s := mustHandler(t)
	if err := s.SetProxyState("frozen:ses_victim", `{"messages":[]}`); err != nil {
		t.Fatalf("seed proxy state: %v", err)
	}
	los58dWithPeer(t, "/usr/bin/python3\x00-c\x00import os")

	for _, prefix := range []string{
		"frozen:",
		"frozen:ses_victim",
		"frozen_markers:",
		"decay:",
		"timestamps:",
		"eagerstub:",
		"sawtooth:",
		"session_model:ses_victim",
		"source_agent:opencode:",
	} {
		resp := h.Handle(Request{Method: "delete_proxy_state_prefix", Params: map[string]any{
			"prefix": prefix, "_peer_pid": 4242,
		}})
		if resp.Error == "" {
			t.Errorf("%s: a client that is not the proxy deleted another session's state", prefix)
		}
	}
	if value, _ := s.GetProxyState("frozen:ses_victim"); value == "" {
		t.Error("a refused delete removed the row")
	}
}

func TestLos58dDeleteProxyStatePrefixAllowsTheProxy(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.SetProxyState("frozen:ses_live", `{"messages":[]}`); err != nil {
		t.Fatalf("seed proxy state: %v", err)
	}
	los58dWithPeer(t, "/home/u/.local/bin/yesmem\x00proxy\x00--port\x004000")

	resp := h.Handle(Request{Method: "delete_proxy_state_prefix", Params: map[string]any{
		"prefix": "frozen:", "_peer_pid": 4242,
	}})
	if resp.Error != "" {
		t.Fatalf("the proxy resets its cache through this call and must keep doing so: %s", resp.Error)
	}
	if value, _ := s.GetProxyState("frozen:ses_live"); value != "" {
		t.Errorf("the proxy's delete did not remove the row: %q", value)
	}
}

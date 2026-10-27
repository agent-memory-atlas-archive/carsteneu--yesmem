package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// YM-427 [PRX-P58]: Los 58d closed the write and delete sides of set_proxy_state
// for the thread-scoped key families, but three gaps stayed open on the same
// surface:
//
//  1. The per-session token threshold was written under
//     config_override:token_threshold:<session>, next to the documented per-model
//     slot. Both are free-form suffixes, and the model segment is chosen by the
//     caller (set_config's per-model alias mirrors it through the same RPC), so the
//     key could not be told apart from one shape alone: whatever the guard learned
//     about "session" suffixes, a caller could make its victim look like a model.
//     The fix gives the session slot its own family, session_override:.
//  2. get_proxy_state answered every key to every caller: whoever knew a key read
//     another session's frozen prefix, decay ages or budget.
//  3. set_config with a session_id wrote the per-session slot for whichever session
//     the caller named, without asking whether the caller is that session.

// los59bWithProcess points the process table at a fixture for one pid: the cmdline
// the peer checks read and, optionally, the environment the peer process carries.
// Both are what the kernel reports, so a test can be a specific caller without
// being a yesmem binary.
func los59bWithProcess(t *testing.T, pid int, cmdline string, env ...string) {
	t.Helper()
	dir := t.TempDir()
	pidDir := filepath.Join(dir, strconv.Itoa(pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("create proc fixture: %v", err)
	}
	if cmdline != "" {
		if err := os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatalf("write cmdline fixture: %v", err)
		}
	}
	if len(env) > 0 {
		payload := strings.Join(env, "\x00") + "\x00"
		if err := os.WriteFile(filepath.Join(pidDir, "environ"), []byte(payload), 0o644); err != nil {
			t.Fatalf("write environ fixture: %v", err)
		}
	}
	prev := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = prev })
}

// los59bForeignPeer is a socket client that is neither the proxy nor a process
// carrying a session: a plain interpreter.
func los59bForeignPeer(t *testing.T) {
	t.Helper()
	los59bWithProcess(t, 4242, "/usr/bin/python3\x00-c\x00import os")
}

// A session threshold is one session's state.
func TestLos59bSetProxyStateRefusesAForeignPeerForASessionThreshold(t *testing.T) {
	h, s := mustHandler(t)
	los59bForeignPeer(t)

	for _, key := range []string{
		"session_override:token_threshold:ses_victim",
		"session_override:token_threshold:opencode:ses_victim",
	} {
		resp := h.Handle(Request{Method: "set_proxy_state", Params: map[string]any{
			"key": key, "value": "1000", "_peer_pid": 4242,
		}})
		if resp.Error == "" {
			t.Errorf("%s: a client that is not the proxy set another session's budget", key)
		}
		if value, _ := s.GetProxyState(key); value != "" {
			t.Errorf("%s: the refused write was stored anyway: %q", key, value)
		}
	}
}

// The model namespace and the proxy itself stay writable: `yesmem config set`
// mirrors a per-model slot through this RPC, and the proxy persists the state it
// serves. A model slot that happens to look like a session id is not session state
// — it lives in a namespace the session reader does not touch (see
// TestLos59bSessionOverrideIgnoresTheModelNamespace in internal/proxy).
func TestLos59bSetProxyStateKeepsTheModelSlotAndTheProxyWritable(t *testing.T) {
	h, s := mustHandler(t)
	los59bWithProcess(t, 4242, "/usr/local/bin/yesmem\x00config\x00set\x00proxy.token_thresholds.glm-5.2\x00500000")

	for _, key := range []string{
		"config_override:token_threshold",
		"config_override:token_threshold:glm-5.2",
		"config_override:token_threshold:ses_looks-like-a-model",
	} {
		resp := h.Handle(Request{Method: "set_proxy_state", Params: map[string]any{
			"key": key, "value": "500000", "_peer_pid": 4242,
		}})
		if resp.Error != "" {
			t.Fatalf("%s: the CLI mirrors the model slot through this RPC: %s", key, resp.Error)
		}
		if value, _ := s.GetProxyState(key); value != "500000" {
			t.Errorf("%s = %q, want the written value", key, value)
		}
	}

	h2, s2 := mustHandler(t)
	los59bWithProcess(t, 4242, "/home/u/.local/bin/yesmem\x00proxy\x00--port\x004000")
	resp := h2.Handle(Request{Method: "set_proxy_state", Params: map[string]any{
		"key": "session_override:token_threshold:ses_live", "value": "300000", "_peer_pid": 4242,
	}})
	if resp.Error != "" {
		t.Fatalf("the proxy persists the override it serves: %s", resp.Error)
	}
	if value, _ := s2.GetProxyState("session_override:token_threshold:ses_live"); value != "300000" {
		t.Errorf("session slot = %q, want the proxy's value", value)
	}
}

// The read side answers the same question as the write side: whoever knows a key
// read another session's state.
func TestLos59bGetProxyStateRefusesAForeignPeerForSessionState(t *testing.T) {
	h, s := mustHandler(t)
	for key, value := range map[string]string{
		"frozen:ses_victim":                           `{"messages":[]}`,
		"session_override:token_threshold:ses_victim": "123456",
		"config_override:token_threshold:deepseek":    "600000",
	} {
		if err := s.SetProxyState(key, value); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	los59bForeignPeer(t)

	for _, key := range []string{
		"frozen:ses_victim",
		"session_override:token_threshold:ses_victim",
	} {
		resp := h.Handle(Request{Method: "get_proxy_state", Params: map[string]any{
			"key": key, "_peer_pid": 4242,
		}})
		if resp.Error == "" {
			t.Errorf("%s: a client that is not the proxy read another session's state", key)
		}
		if strings.Contains(string(resp.Result), "123456") || strings.Contains(string(resp.Result), "messages") {
			t.Errorf("%s: the refusal still carried the value: %s", key, resp.Result)
		}
	}

	// The model slot is global configuration, and get_config answers it to every
	// caller; refusing the read here would hide nothing and break the mirror.
	resp := h.Handle(Request{Method: "get_proxy_state", Params: map[string]any{
		"key": "config_override:token_threshold:deepseek", "_peer_pid": 4242,
	}})
	if resp.Error != "" {
		t.Fatalf("the model slot stays readable: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "600000") {
		t.Errorf("model slot read = %s, want the value", resp.Result)
	}
}

// A call without a peer is the daemon's own code, and the guard abstains there.
func TestLos59bGetProxyStateKeepsInternalReadsWorking(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.SetProxyState("frozen:ses_internal", `{"messages":[]}`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := h.Handle(Request{Method: "get_proxy_state", Params: map[string]any{
		"key": "frozen:ses_internal",
	}})
	if resp.Error != "" {
		t.Fatalf("an internal read must not need a peer: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "messages") {
		t.Errorf("internal read = %s, want the stored value", resp.Result)
	}
}

// The destructive sibling reaches the same per-session rows: a prefix delete would
// move the attack one call over. The store matches "everything starting with the
// given string", so a shortened prefix wipes the family just as well.
func TestLos59bDeleteProxyStatePrefixRefusesAForeignPeerForTheThresholdFamily(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.SetProxyState("session_override:token_threshold:ses_victim", "123456"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	los59bForeignPeer(t)

	for _, prefix := range []string{
		"session_override:",
		"session_override:token_threshold",
		"session_override:token_threshold:",
		"session_override",
		"session_over",
		"sess",
	} {
		resp := h.Handle(Request{Method: "delete_proxy_state_prefix", Params: map[string]any{
			"prefix": prefix, "_peer_pid": 4242,
		}})
		if resp.Error == "" {
			t.Errorf("%s: a client that is not the proxy deleted per-session overrides", prefix)
		}
	}
	if value, _ := s.GetProxyState("session_override:token_threshold:ses_victim"); value == "" {
		t.Error("a refused delete removed the row")
	}
}

// set_config with a session_id writes the same slot for a session the caller
// names. The caller's own session is resolved from what the kernel reports, so an
// agent that names somebody else's session is refused.
func TestLos59bSetConfigRefusesAnOverrideForAForeignSession(t *testing.T) {
	h, s := mustHandler(t)
	los59bWithProcess(t, 4242, "/home/u/.local/bin/yesmem\x00mcp", "YESMEM_SESSION_ID=opencode:ses_own")

	resp := h.Handle(Request{Method: "set_config", Params: map[string]any{
		"key":         "token_threshold",
		"value":       "300000",
		"session_id":  "ses_victim",
		"_peer_pid":   4242,
		"_session_id": "opencode:ses_victim",
	}})
	if resp.Error == "" {
		t.Error("an agent set the budget of a session it is not")
	}
	if value, _ := s.GetProxyState("session_override:token_threshold:ses_victim"); value != "" {
		t.Errorf("the refused override was stored: %q", value)
	}
}

// The self-report in the request is not evidence: a caller that claims a session it
// does not run in is refused just the same. The fixture mirrors the measured
// production layout of an opencode v2 session: the MCP child of the shared service
// carries no session in its environment and has no agent row, so nothing binds it.
func TestLos59bSetConfigRefusesAForgedSelfReport(t *testing.T) {
	h, s := mustHandler(t)
	los59bWithProcess(t, 4242, "/home/u/.local/bin/yesmem\x00mcp")

	resp := h.Handle(Request{Method: "set_config", Params: map[string]any{
		"key":         "token_threshold",
		"value":       "300000",
		"session_id":  "ses_victim",
		"_peer_pid":   4242,
		"_session_id": "opencode:ses_victim",
	}})
	if resp.Error == "" {
		t.Error("a request field alone authorized a write into another session's slot")
	}
	if value, _ := s.GetProxyState("session_override:token_threshold:ses_victim"); value != "" {
		t.Errorf("the forged write was stored: %q", value)
	}
}

// The documented case stays working where the daemon can bind the caller: a harness
// that starts its MCP child with its session in the environment (Claude Code, codex,
// a yesmem-spawned agent). An opencode session behind the shared service carries no
// such value, so it is refused and pointed at the global threshold — the narrowing
// this fix buys, and the one the DONE report carries.
func TestLos59bSetConfigKeepsTheCallersOwnSessionWorking(t *testing.T) {
	h, s := mustHandler(t)
	los59bWithProcess(t, 4242, "/home/u/.local/bin/yesmem\x00mcp", "YESMEM_SESSION_ID=opencode:ses_own")

	resp := h.Handle(Request{Method: "set_config", Params: map[string]any{
		"key":        "token_threshold",
		"value":      "300000",
		"session_id": "ses_own",
		"_peer_pid":  4242,
	}})
	if resp.Error != "" {
		t.Fatalf("a session must be able to override itself: %s", resp.Error)
	}
	if value, _ := s.GetProxyState("session_override:token_threshold:ses_own"); value != "300000" {
		t.Errorf("own override = %q, want 300000", value)
	}
}

// A write without a peer is the daemon's own code (and the CLI), and the guard
// abstains there exactly as it does on set_proxy_state.
func TestLos59bSetConfigKeepsInternalSessionWritesWorking(t *testing.T) {
	h, s := mustHandler(t)

	resp := h.Handle(Request{Method: "set_config", Params: map[string]any{
		"key":        "token_threshold",
		"value":      "300000",
		"session_id": "ses_internal",
	}})
	if resp.Error != "" {
		t.Fatalf("an internal session override must not need a peer: %s", resp.Error)
	}
	if value, _ := s.GetProxyState("session_override:token_threshold:ses_internal"); value != "300000" {
		t.Errorf("internal override = %q, want 300000", value)
	}
}

// Reading a session override names a session just like writing one, and the value
// is the same budget. get_config is the sibling the write guard would otherwise
// leave open (cold review finding).
func TestLos59bGetConfigRefusesAForeignSessionsOverride(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.SetProxyState("session_override:token_threshold:ses_victim", "123456"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	los59bWithProcess(t, 4242, "/home/u/.local/bin/yesmem\x00mcp", "YESMEM_SESSION_ID=opencode:ses_own")

	resp := h.Handle(Request{Method: "get_config", Params: map[string]any{
		"key":        "token_threshold",
		"session_id": "ses_victim",
		"_peer_pid":  4242,
	}})
	if resp.Error == "" {
		t.Error("an agent read the budget of a session it is not")
	}
	if strings.Contains(string(resp.Result), "123456") {
		t.Errorf("the refusal carried the value: %s", resp.Result)
	}
}

// Its own session reads its own override, in both spellings the proxy accepts.
func TestLos59bGetConfigKeepsTheCallersOwnReadWorking(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.SetProxyState("session_override:token_threshold:ses_own", "123456"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	los59bWithProcess(t, 4242, "/home/u/.local/bin/yesmem\x00mcp", "YESMEM_SESSION_ID=opencode:ses_own")

	for _, named := range []string{"ses_own", "opencode:ses_own"} {
		resp := h.Handle(Request{Method: "get_config", Params: map[string]any{
			"key": "token_threshold", "session_id": named, "_peer_pid": 4242,
		}})
		if resp.Error != "" {
			t.Fatalf("%s: a session must be able to read its own override: %s", named, resp.Error)
		}
		if !strings.Contains(string(resp.Result), "123456") {
			t.Errorf("%s: own read = %s, want the value", named, resp.Result)
		}
	}
}

// A caller that cannot be identified at all is refused rather than served the
// global value under a session scope.
func TestLos59bGetConfigRefusesAnUnidentifiableCaller(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.SetProxyState("session_override:token_threshold:ses_own", "123456"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	los59bForeignPeer(t)

	resp := h.Handle(Request{Method: "get_config", Params: map[string]any{
		"key": "token_threshold", "session_id": "ses_own", "_peer_pid": 4242,
	}})
	if resp.Error == "" {
		t.Errorf("an unidentifiable caller read a session override: %s", resp.Result)
	}
}

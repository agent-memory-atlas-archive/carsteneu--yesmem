package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/httpapi"
	"github.com/carsteneu/yesmem/internal/storage"
)

func TestHandleSendTo_WithMsgType(t *testing.T) {
	h, s := mustHandler(t)
	resp := h.Handle(Request{
		Method: "send_to",
		Params: map[string]any{
			"target":   "target-session-1",
			"content":  "task done",
			"sender":   "sender-session-1",
			"msg_type": "response",
		},
	})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	msgs, err := s.GetChannelMessages([]string{"target-session-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].MsgType != "response" {
		t.Errorf("msg_type=%q want response", msgs[0].MsgType)
	}
}

func TestHandleSendTo_DefaultMsgType(t *testing.T) {
	h, s := mustHandler(t)
	resp := h.Handle(Request{
		Method: "send_to",
		Params: map[string]any{
			"target":  "target-session-2",
			"content": "do this task",
			"sender":  "sender-session-2",
		},
	})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	msgs, err := s.GetChannelMessages([]string{"target-session-2"})
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].MsgType != "command" {
		t.Errorf("msg_type=%q want command (default)", msgs[0].MsgType)
	}
}

func TestHandleSendTo_AckOnAckDropped(t *testing.T) {
	h, _ := mustHandler(t)

	// First: A sends ack to B
	h.Handle(Request{
		Method: "send_to",
		Params: map[string]any{
			"target": "session-B", "content": "ok",
			"sender": "session-A", "msg_type": "ack",
		},
	})

	// B sends ack back to A → ACK-on-ACK, should be dropped
	resp := h.Handle(Request{
		Method: "send_to",
		Params: map[string]any{
			"target": "session-A", "content": "acknowledged",
			"sender": "session-B", "msg_type": "ack",
		},
	})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	var result map[string]any
	json.Unmarshal(resp.Result, &result)
	if result["ack_dropped"] != true {
		t.Error("expected ack_dropped=true for ACK-on-ACK")
	}
}

// --- resolveSessionID ---

func TestResolveSessionID_DirectParam(t *testing.T) {
	h, _ := mustHandler(t)
	got := h.resolveSessionID(map[string]any{"session_id": "direct-123"}, "session_id")
	if got != "direct-123" {
		t.Errorf("got %q, want direct-123", got)
	}
}

func TestResolveSessionID_FallbackSessionID(t *testing.T) {
	h, _ := mustHandler(t)
	got := h.resolveSessionID(map[string]any{"_session_id": "fallback-456"}, "missing_key")
	if got != "fallback-456" {
		t.Errorf("got %q, want fallback-456", got)
	}
}

func TestResolveSessionID_PIDLookup(t *testing.T) {
	h, _ := mustHandler(t)
	h.pidMapMu.Lock()
	h.pidMap["pid-session-789"] = 42
	h.pidMapMu.Unlock()

	got := h.resolveSessionID(map[string]any{"_caller_pid": float64(42)}, "missing")
	if got != "pid-session-789" {
		t.Errorf("got %q, want pid-session-789", got)
	}
}

func TestResolveSessionID_ActiveSessionFallback(t *testing.T) {
	h, _ := mustHandler(t)
	h.activeSessionMu.Lock()
	h.activeSessionID = "active-fallback"
	h.activeSessionMu.Unlock()

	got := h.resolveSessionID(map[string]any{}, "missing")
	if got != "active-fallback" {
		t.Errorf("got %q, want active-fallback", got)
	}
}

// --- resolveSessionID: ambiguous PIDs ---
//
// The opencode plugin registers the PID that hosts a session. When one PID is
// registered for several sessions (shared service process, PID-file rebuild),
// returning whichever session the map yields first misattributes the call, so
// the resolver must refuse and let the caller-cwd path decide instead.

func TestResolveSessionID_AmbiguousPIDResolvesNothing(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	h.pidMapMu.Lock()
	h.pidMap["opencode:ses_amb_a"] = 4242
	h.pidMap["opencode:ses_amb_b"] = 4242
	h.pidMapMu.Unlock()

	got := h.resolveSessionID(map[string]any{"_caller_pid": float64(4242)}, "missing")
	if got != "" {
		t.Fatalf("got %q, want no resolution for a PID mapping to two sessions", got)
	}
}

func TestResolveSessionIDFromPID_AmbiguousPIDResolvesNothing(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)
	h.pidMapMu.Lock()
	h.pidMap["opencode:ses_amb_c"] = 4243
	h.pidMap["opencode:ses_amb_d"] = 4243
	h.pidMapMu.Unlock()

	if got := h.resolveSessionIDFromPID(4243); got != "" {
		t.Fatalf("got %q, want no resolution for a PID mapping to two sessions", got)
	}
}

// A PID with exactly one session keeps resolving — the rejection is about
// ambiguity, not about PID lookups in general.
func TestResolveSessionID_UnambiguousPIDStillResolves(t *testing.T) {
	h, _ := mustHandler(t)
	h.pidMapMu.Lock()
	h.pidMap["opencode:ses_only"] = 4244
	h.pidMapMu.Unlock()

	got := h.resolveSessionID(map[string]any{"_caller_pid": float64(4244)}, "missing")
	if got != "opencode:ses_only" {
		t.Fatalf("got %q, want opencode:ses_only", got)
	}
}

// --- resolveSessionID DB PID fallback (parallel-safe agent resolution) ---
// When pidMap (in-memory) and PID file (on-disk) misses, the resolver must
// consult the agents table directly: WHERE pid = ? AND status='running'.
// This closes the gap for parallel agents where the global
// active_session_opencode proxy-state is Last-Writer-Wins and would route
// every agent's whoami() to whichever agent last made a proxy request.

func TestResolveSessionID_PIDDBFallback(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-pid-db", Project: "proj", Section: "sec",
		SessionID: "pid-db-sess", PID: 11111,
		Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	// No pidMap entry, no PID file — DB fallback must resolve.
	got := h.resolveSessionID(map[string]any{"_caller_pid": float64(11111)}, "missing")
	if got != "pid-db-sess" {
		t.Errorf("got %q, want pid-db-sess (DB PID fallback)", got)
	}
}

func TestResolveSessionID_PIDDBFallback_SkipsStopped(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-pid-stopped", Project: "proj", Section: "sec",
		SessionID: "stopped-sess", PID: 22222,
		Status: "stopped",
	}); err != nil {
		t.Fatal(err)
	}
	// Stopped agent must NOT match (PID reuse protection).
	got := h.resolveSessionID(map[string]any{"_caller_pid": float64(22222)}, "missing")
	if got == "stopped-sess" {
		t.Errorf("resolver matched stopped agent — PID reuse risk")
	}
}

func TestResolveSessionID_PIDDBFallback_PrefersInMemoryMap(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-db", Project: "proj", Section: "sec",
		SessionID: "db-sess", PID: 33333,
		Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	// Populate pidMap with a different session for the same PID.
	// The in-memory map must win over the DB fallback so explicit registerPID
	// calls remain authoritative for Claude-Code flow.
	h.pidMapMu.Lock()
	h.pidMap["in-memory-sess"] = 33333
	h.pidMapMu.Unlock()

	got := h.resolveSessionID(map[string]any{"_caller_pid": float64(33333)}, "missing")
	if got != "in-memory-sess" {
		t.Errorf("got %q, want in-memory-sess (pidMap must outrank DB)", got)
	}
}

func TestResolveSessionID_PIDDBFallback_NoCallerPID(t *testing.T) {
	h, _ := mustHandler(t)
	// No _caller_pid in params — DB fallback must not fire. Should fall
	// through to downstream logic (active-session fallback etc.) without
	// panic and without matching any agent row via PID 0.
	got := h.resolveSessionID(map[string]any{}, "missing")
	// With no active session and no caller PID, resolver must return "" —
	// a non-empty value here would indicate a spurious match against a
	// freshly-spawned row that has not yet set agents.pid.
	if got != "" {
		t.Errorf("resolveSessionID without _caller_pid or active session must return empty, got %q", got)
	}
}

// --- Parallel-safe resolution: two concurrent non-claude agents ---
// The core invariant this whole change protects: two opencode agents running
// at the same time, with distinct PIDs, must resolve to their own identities
// — not whichever last touched the global proxy-state. This test would fail
// under the old Last-Writer-Wins active_session_opencode fallback because
// both PIDs would resolve to whichever session the proxy-state held.

func TestResolveSessionID_PIDDBFallback_TwoParallelAgents(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-alpha", Project: "proj-a", Section: "sec-a",
		SessionID: "alpha-sess", PID: 44441,
		Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-beta", Project: "proj-b", Section: "sec-b",
		SessionID: "beta-sess", PID: 44442,
		Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	// Alpha resolves to alpha-sess
	if got := h.resolveSessionID(map[string]any{"_caller_pid": float64(44441)}, "missing"); got != "alpha-sess" {
		t.Errorf("PID 44441: want alpha-sess, got %q", got)
	}
	// Beta resolves to beta-sess — not hijacked by alpha's prior resolution
	if got := h.resolveSessionID(map[string]any{"_caller_pid": float64(44442)}, "missing"); got != "beta-sess" {
		t.Errorf("PID 44442: want beta-sess, got %q", got)
	}
	// And re-resolving alpha still works (no LWW contamination)
	if got := h.resolveSessionID(map[string]any{"_caller_pid": float64(44441)}, "missing"); got != "alpha-sess" {
		t.Errorf("PID 44441 second call: want alpha-sess, got %q (parallel resolution not stable)", got)
	}
}

// --- handleStartDialog ---

func TestHandleStartDialog_OK(t *testing.T) {
	h, _ := mustHandler(t)
	h.activeSessionMu.Lock()
	h.activeSessionID = "initiator-1"
	h.activeSessionMu.Unlock()

	resp := h.handleStartDialog(map[string]any{"partner": "partner-1", "topic": "test topic"})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["status"] != "pending" {
		t.Errorf("status=%v, want pending", m["status"])
	}
	if m["dialog_id"] == nil || m["dialog_id"].(float64) == 0 {
		t.Error("expected non-zero dialog_id")
	}
}

func TestHandleStartDialog_MissingParams(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleStartDialog(map[string]any{"partner": "p"})
	if resp.Error == "" {
		t.Error("expected error for missing initiator")
	}

	h.activeSessionMu.Lock()
	h.activeSessionID = "init-1"
	h.activeSessionMu.Unlock()
	resp = h.handleStartDialog(map[string]any{})
	if resp.Error == "" {
		t.Error("expected error for missing partner")
	}
}

// --- handleSendTo additional cases ---

func TestHandleSendTo_MissingParams(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{"target": "t"}})
	if resp.Error == "" {
		t.Error("expected error for missing content")
	}
	resp = h.Handle(Request{Method: "send_to", Params: map[string]any{"content": "c"}})
	if resp.Error == "" {
		t.Error("expected error for missing target")
	}
}

// A caller that proves no identity must not be labelled with the
// last-writer-wins active session: that is whichever session wrote last, so the
// replies to the label reached a foreign session (YM-60).
func TestHandleSendTo_UnidentifiedSenderIsNotTheActiveSession(t *testing.T) {
	h, s := mustHandler(t)
	h.activeSessionMu.Lock()
	h.activeSessionID = "foreign-session"
	h.activeSessionMu.Unlock()

	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{"target": "tgt-1", "content": "hello"}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	msgs, _ := s.GetChannelMessages([]string{"tgt-1"})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(msgs))
	}
	if msgs[0].Sender == "foreign-session" {
		t.Errorf("sender=%q — a caller that proved nothing was labelled with the last active session", msgs[0].Sender)
	}
	if msgs[0].Sender != "unknown" {
		t.Errorf("sender=%q, want unknown", msgs[0].Sender)
	}
}

func TestHandleSendTo_ImplicitAck(t *testing.T) {
	h, s := mustHandler(t)
	// Send a message to sender-X first
	s.SendChannelMessage("sender-X", "other", "incoming msg", "command")

	// When sender-X sends a reply, their incoming messages should be marked read
	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "other", "content": "reply", "sender": "sender-X",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}

	// The message targeted at sender-X should now be delivered/read
	msgs, _ := s.GetChannelMessages([]string{"sender-X"})
	if len(msgs) != 0 {
		t.Errorf("expected 0 undelivered messages after implicit ack, got %d", len(msgs))
	}
}

// --- handleCheckChannel ---

func TestHandleCheckChannel_OK(t *testing.T) {
	h, s := mustHandler(t)
	s.SendChannelMessage("sess-chan-1", "sender-a", "msg one", "command")
	s.SendChannelMessage("sess-chan-1", "sender-b", "msg two", "status")

	resp := h.Handle(Request{Method: "check_channel", Params: map[string]any{"session_id": "sess-chan-1"}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["content"] != "msg one" {
		t.Errorf("content=%v, want msg one", first["content"])
	}
	if first["msg_type"] != "command" {
		t.Errorf("msg_type=%v, want command", first["msg_type"])
	}
}

func TestHandleCheckChannel_Empty(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "check_channel", Params: map[string]any{"session_id": "no-messages"}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	msgs := m["messages"].([]any)
	if len(msgs) != 0 {
		t.Errorf("expected empty messages, got %d", len(msgs))
	}
}

func TestHandleCheckChannel_MissingSessionID(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "check_channel", Params: map[string]any{}})
	if resp.Error == "" {
		t.Error("expected error for missing session_id")
	}
}

// --- handleMarkChannelRead ---

func TestHandleMarkChannelRead_OK(t *testing.T) {
	h, s := mustHandler(t)
	s.SendChannelMessage("sess-mark-1", "other", "unread msg", "command")

	resp := h.Handle(Request{Method: "mark_channel_read", Params: map[string]any{"session_id": "sess-mark-1"}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["status"] != "ok" {
		t.Errorf("status=%v, want ok", m["status"])
	}
	// Verify messages are now read
	msgs, _ := s.GetChannelMessages([]string{"sess-mark-1"})
	if len(msgs) != 0 {
		t.Errorf("expected 0 undelivered after mark_channel_read, got %d", len(msgs))
	}
}

func TestHandleMarkChannelRead_MissingSessionID(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "mark_channel_read", Params: map[string]any{}})
	if resp.Error == "" {
		t.Error("expected error for missing session_id")
	}
}

// --- handleRegisterPID ---

func TestHandleListRegistrations(t *testing.T) {
	h, _ := mustHandler(t)
	h.Handle(Request{Method: "register_pid", Params: map[string]any{"session_id": "opencode:ses-1", "pid": float64(12345), "source_agent": "opencode"}})
	h.Handle(Request{Method: "register_window", Params: map[string]any{"session_id": "opencode:ses-1", "window_id": "0x5600004", "terminal": "ghostty"}})

	resp := h.Handle(Request{Method: "list_registrations", Params: map[string]any{}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Result, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sessions, _ := m["sessions"].(map[string]any)
	if sessions["opencode:ses-1"] != float64(12345) {
		t.Errorf("sessions = %v", sessions)
	}
	windows, _ := m["windows"].(map[string]any)
	if windows["opencode:ses-1"] != "0x5600004" {
		t.Errorf("windows = %v", windows)
	}
	terms, _ := m["terminals"].(map[string]any)
	if terms["opencode:ses-1"] != "ghostty" {
		t.Errorf("terminals = %v", terms)
	}
}

func TestHandleRegisterPID_OK(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "register_pid", Params: map[string]any{"session_id": "pid-sess-1", "pid": float64(12345)}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}

	h.pidMapMu.Lock()
	pid := h.pidMap["pid-sess-1"]
	h.pidMapMu.Unlock()
	if pid != 12345 {
		t.Errorf("pid=%d, want 12345", pid)
	}

	// Also updates activeSessionID
	h.activeSessionMu.Lock()
	active := h.activeSessionID
	h.activeSessionMu.Unlock()
	if active != "pid-sess-1" {
		t.Errorf("activeSessionID=%q, want pid-sess-1", active)
	}
}

func TestHandleRegisterPID_MissingParams(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "register_pid", Params: map[string]any{"session_id": "s"}})
	if resp.Error == "" {
		t.Error("expected error for missing pid")
	}
	resp = h.Handle(Request{Method: "register_pid", Params: map[string]any{"pid": float64(1)}})
	if resp.Error == "" {
		t.Error("expected error for missing session_id")
	}
}

func TestHandleRegisterPID_WithSourceAgent(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "register_pid", Params: map[string]any{
		"session_id":   "opencode:ses-x",
		"pid":          float64(99999),
		"source_agent": "opencode",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}

	val, err := h.store.GetProxyState("source_agent:opencode:ses-x")
	if err != nil {
		t.Fatal(err)
	}
	if val != "opencode" {
		t.Errorf("source_agent proxy state = %q, want opencode", val)
	}
}

// --- handleRegisterWindow ---

func TestHandleRegisterWindow_OK(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "register_window", Params: map[string]any{
		"session_id": "win-sess-1", "window_id": "0x1234", "terminal": "ghostty",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}

	h.windowMapMu.Lock()
	wid := h.windowMap["win-sess-1"]
	term := h.terminalMap["win-sess-1"]
	h.windowMapMu.Unlock()
	if wid != "0x1234" {
		t.Errorf("window_id=%q, want 0x1234", wid)
	}
	if term != "ghostty" {
		t.Errorf("terminal=%q, want ghostty", term)
	}
}

func TestHandleRegisterWindow_NoTerminal(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "register_window", Params: map[string]any{
		"session_id": "win-sess-2", "window_id": "0x5678",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	h.windowMapMu.Lock()
	_, hasTerm := h.terminalMap["win-sess-2"]
	h.windowMapMu.Unlock()
	if hasTerm {
		t.Error("expected no terminal entry when not provided")
	}
}

func TestHandleRegisterWindow_MissingParams(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "register_window", Params: map[string]any{"session_id": "s"}})
	if resp.Error == "" {
		t.Error("expected error for missing window_id")
	}
	resp = h.Handle(Request{Method: "register_window", Params: map[string]any{"window_id": "w"}})
	if resp.Error == "" {
		t.Error("expected error for missing session_id")
	}
}

// --- handleBroadcast ---

func TestHandleBroadcast_OK(t *testing.T) {
	h, s := mustHandler(t)

	// The label is resolved from the kernel-reported peer, so the caller needs a row
	// whose pid the daemon can find (YM-271). The active-session fallback is gone on
	// this path, and a session id the caller sends is not evidence.
	peer := os.Getpid()
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-20261020-80", Project: "test-proj", Section: "bc",
		SessionID: "bc-sender", Status: "running", PID: peer,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.Handle(Request{Method: "broadcast", Params: map[string]any{
		"project": "test-proj", "content": "hello everyone",
		"_cwd": "/srv/test-proj", "_peer_pid": peer,
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["status"] != "broadcast_sent" {
		t.Errorf("status=%v, want broadcast_sent", m["status"])
	}
	if m["message_id"] == nil || m["message_id"].(float64) == 0 {
		t.Error("expected non-zero message_id")
	}
}

func TestHandleBroadcast_MissingParams(t *testing.T) {
	h, _ := mustHandler(t)
	h.activeSessionMu.Lock()
	h.activeSessionID = "bc-sender"
	h.activeSessionMu.Unlock()

	resp := h.Handle(Request{Method: "broadcast", Params: map[string]any{"content": "c"}})
	if resp.Error == "" {
		t.Error("expected error for missing project")
	}
	resp = h.Handle(Request{Method: "broadcast", Params: map[string]any{"project": "p"}})
	if resp.Error == "" {
		t.Error("expected error for missing content")
	}
}

func TestHandleBroadcast_MissingSender(t *testing.T) {
	h, _ := mustHandler(t)
	// No activeSessionID, no sender param
	resp := h.Handle(Request{Method: "broadcast", Params: map[string]any{"project": "p", "content": "c"}})
	if resp.Error == "" {
		t.Error("expected error for missing sender")
	}
}

// --- handleCheckBroadcasts ---

func TestHandleCheckBroadcasts_OK(t *testing.T) {
	h, s := mustHandler(t)
	s.SendBroadcast("sender-bc", "/srv/proj-1", "broadcast msg")

	resp := h.Handle(Request{Method: "check_broadcasts", Params: map[string]any{
		"session_id": "reader-1", "project": "proj-1", "_cwd": "/srv/proj-1",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["content"] != "broadcast msg" {
		t.Errorf("content=%v, want broadcast msg", first["content"])
	}
}

func TestHandleCheckBroadcasts_Empty(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "check_broadcasts", Params: map[string]any{
		"session_id": "reader-2", "project": "proj-2", "_cwd": "/srv/proj-2",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	msgs := m["messages"].([]any)
	if len(msgs) != 0 {
		t.Errorf("expected empty, got %d", len(msgs))
	}
}

func TestHandleCheckBroadcasts_MissingParams(t *testing.T) {
	h, _ := mustHandler(t)
	// A broadcast read identifies its reader by the session id it names, so a
	// caller that names none cannot be served at all (YM-235, same form as the
	// endpoints YM-204 closed). The empty-project case stays a no-op.
	resp := h.Handle(Request{Method: "check_broadcasts", Params: map[string]any{}})
	if resp.Error == "" {
		t.Error("expected an error for a broadcast read without a session id")
	}
	resp = h.Handle(Request{Method: "check_broadcasts", Params: map[string]any{"session_id": "reader-x"}})
	if resp.Error != "" {
		t.Fatalf("a missing project must stay a no-op: %s", resp.Error)
	}
	m := resultMap(t, resp)
	if msgs := m["messages"].([]any); len(msgs) != 0 {
		t.Errorf("expected empty, got %d", len(msgs))
	}
}

func TestHandleCheckBroadcasts_MarksRead(t *testing.T) {
	h, s := mustHandler(t)
	s.SendBroadcast("sender-x", "/srv/proj-r", "first read")

	// First check: should see the broadcast
	resp := h.Handle(Request{Method: "check_broadcasts", Params: map[string]any{
		"session_id": "reader-r", "project": "proj-r", "_cwd": "/srv/proj-r",
	}})
	m := resultMap(t, resp)
	if len(m["messages"].([]any)) != 1 {
		t.Fatal("expected 1 broadcast on first check")
	}

	// Second check: should be empty (already read)
	resp = h.Handle(Request{Method: "check_broadcasts", Params: map[string]any{
		"session_id": "reader-r", "project": "proj-r", "_cwd": "/srv/proj-r",
	}})
	m = resultMap(t, resp)
	if len(m["messages"].([]any)) != 0 {
		t.Error("expected 0 broadcasts after re-read")
	}
}

// A broadcast read names its reader and then marks what it showed as read, so
// it carried the same ownership hole the channel inbox had before YM-204: any
// socket client could name another session and consume its broadcasts. The peer
// guard closes it (YM-235).
func TestCheckBroadcasts_ForeignCallerStaysClosed(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendBroadcast("orchestrator-b", "proj-b", "broadcast for the victim"); err != nil {
		t.Fatal(err)
	}
	stubProcCmdline(t, "/usr/bin/opencode\x00serve\x00")

	for name, extra := range map[string]map[string]any{
		"an identity it asserts itself": {"_session_id": "opencode:ses_VICTIM_BC"},
		"its own unrelated identity":    {"_session_id": "caller-session"},
		"nothing at all":                {},
	} {
		params := map[string]any{
			"session_id": "opencode:ses_VICTIM_BC",
			"project":    "proj-b",
			"_peer_pid":  4321,
		}
		for k, v := range extra {
			params[k] = v
		}
		resp := h.Handle(Request{Method: "check_broadcasts", Params: params})
		if resp.Error == "" {
			t.Errorf("%s read a foreign broadcast inbox: %s", name, resp.Result)
		}
		if strings.Contains(string(resp.Result), "broadcast for the victim") {
			t.Errorf("%s received the victim's broadcast: %s", name, resp.Result)
		}
	}

	left, err := s.GetUnreadBroadcasts("opencode:ses_VICTIM_BC", "proj-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("a foreign caller consumed the victim's broadcast: %d left", len(left))
	}
}

// The guard must not close the proxy out: it is the only reader of these
// endpoints, and a proxy binary whose argv no longer matches would stop channel
// delivery entirely (the failure is logged, not silent).
func TestCheckBroadcasts_ProxyCallerIsServed(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendBroadcast("orchestrator-c", "/srv/proj-c", "broadcast for the reader"); err != nil {
		t.Fatal(err)
	}
	stubProcCmdline(t, "/home/u/.local/bin/yesmem\x00proxy\x00")

	resp := h.Handle(Request{Method: "check_broadcasts", Params: map[string]any{
		"session_id": "opencode:ses_READER_C", "project": "/srv/proj-c", "_peer_pid": 4321,
	}})
	if resp.Error != "" {
		t.Fatalf("the proxy was refused its own consumer path: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "broadcast for the reader") {
		t.Errorf("the proxy did not receive the broadcast: %s", resp.Result)
	}

	// The identity is still required: absence of the session id is refused for
	// that reason, not because of the peer.
	resp = h.Handle(Request{Method: "check_broadcasts", Params: map[string]any{
		"project": "/srv/proj-c", "_peer_pid": 4321,
	}})
	if !strings.Contains(resp.Error, "session_id") {
		t.Errorf("a proxy call without a session id failed for the wrong reason: %q", resp.Error)
	}
}

// --- handleEndDialog ---

func TestHandleEndDialog_ByDialogID(t *testing.T) {
	h, s := mustHandler(t)
	id, _ := s.StartDialog("init-e", "part-e", "ending test")

	resp := h.handleEndDialog(map[string]any{"dialog_id": float64(id)})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["status"] != "ended" {
		t.Errorf("status=%v, want ended", m["status"])
	}
}

func TestHandleEndDialog_BySession(t *testing.T) {
	h, s := mustHandler(t)
	s.StartDialog("init-es", "part-es", "session lookup")

	resp := h.handleEndDialog(map[string]any{"session_id": "init-es"})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["status"] != "ended" {
		t.Errorf("status=%v, want ended", m["status"])
	}
}

func TestHandleEndDialog_NoDialog(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleEndDialog(map[string]any{"session_id": "no-dialog-sess"})
	if resp.Error == "" {
		t.Error("expected error for no active dialog")
	}
}

func TestHandleEndDialog_MissingParams(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleEndDialog(map[string]any{})
	if resp.Error == "" {
		t.Error("expected error for missing dialog_id and session_id")
	}
}

// --- handleCheckInvitations ---

func TestHandleCheckInvitations_None(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleCheckInvitations(map[string]any{"session_id": "no-inv-sess"})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["has_invitation"] != false {
		t.Error("expected has_invitation=false")
	}
}

func TestHandleCheckInvitations_WithPending(t *testing.T) {
	h, s := mustHandler(t)
	dialogID, _ := s.StartDialog("init-inv", "partner-inv", "collab topic")
	// Add an initial message
	s.SendDialogMessage(dialogID, "init-inv", "let's work together")

	resp := h.handleCheckInvitations(map[string]any{"session_id": "partner-inv"})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["has_invitation"] != true {
		t.Error("expected has_invitation=true")
	}
	if m["initiator"] != "init-inv" {
		t.Errorf("initiator=%v, want init-inv", m["initiator"])
	}
	if m["topic"] != "collab topic" {
		t.Errorf("topic=%v, want collab topic", m["topic"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 initial message, got %d", len(msgs))
	}
	if msgs[0] != "let's work together" {
		t.Errorf("message=%v, want let's work together", msgs[0])
	}
}

func TestHandleCheckInvitations_MissingSessionID(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleCheckInvitations(map[string]any{})
	if resp.Error == "" {
		t.Error("expected error for missing session_id")
	}
}

// --- handleCheckMessages ---

func TestHandleCheckMessages_NoDialog(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleCheckMessages(map[string]any{"session_id": "no-dialog-cm"})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["has_dialog"] != false {
		t.Error("expected has_dialog=false")
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 0 {
		t.Errorf("expected empty messages, got %d", len(msgs))
	}
}

func TestHandleCheckMessages_WithMessages(t *testing.T) {
	h, s := mustHandler(t)
	dialogID, _ := s.StartDialog("init-cm", "partner-cm", "messages topic")
	s.SendDialogMessage(dialogID, "init-cm", "hello partner")
	s.SendDialogMessage(dialogID, "init-cm", "second msg")

	resp := h.handleCheckMessages(map[string]any{"session_id": "partner-cm"})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["has_dialog"] != true {
		t.Error("expected has_dialog=true")
	}
	if m["topic"] != "messages topic" {
		t.Errorf("topic=%v, want messages topic", m["topic"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}

func TestHandleCheckMessages_MissingSessionID(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleCheckMessages(map[string]any{})
	if resp.Error == "" {
		t.Error("expected error for missing session_id")
	}
}

// --- handleMarkRead ---

func TestHandleMarkRead_OK(t *testing.T) {
	h, s := mustHandler(t)
	dialogID, _ := s.StartDialog("init-mr", "partner-mr", "mark read topic")
	s.SendDialogMessage(dialogID, "init-mr", "unread msg")

	resp := h.handleMarkRead(map[string]any{
		"dialog_id": float64(dialogID), "session_id": "partner-mr",
	})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["status"] != "ok" {
		t.Errorf("status=%v, want ok", m["status"])
	}

	// Verify messages are now read
	msgs, _ := s.GetUnreadMessages(dialogID, "partner-mr")
	if len(msgs) != 0 {
		t.Errorf("expected 0 unread after mark_read, got %d", len(msgs))
	}
}

func TestHandleMarkRead_MissingParams(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleMarkRead(map[string]any{"dialog_id": float64(1)})
	if resp.Error == "" {
		t.Error("expected error for missing session_id")
	}
	resp = h.handleMarkRead(map[string]any{"session_id": "s"})
	if resp.Error == "" {
		t.Error("expected error for missing dialog_id")
	}
}

// --- handleWhoami ---

func TestHandleWhoami_NonAgent(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "whoami", Params: map[string]any{"session_id": "regular-sess", "project": "proj-w"}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["session_id"] != "regular-sess" {
		t.Errorf("session_id=%v, want regular-sess", m["session_id"])
	}
	if m["project"] != "proj-w" {
		t.Errorf("project=%v, want proj-w", m["project"])
	}
	if m["is_agent"] != false {
		t.Error("expected is_agent=false for non-agent session")
	}
}

func TestHandleWhoami_Agent(t *testing.T) {
	h, s := mustHandler(t)
	err := s.AgentCreate(storage.Agent{
		ID: "agent-001", Project: "proj-a", Section: "testing",
		SessionID: "agent-sess-1", Status: "running",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := h.Handle(Request{Method: "whoami", Params: map[string]any{"session_id": "agent-sess-1", "project": "proj-a"}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["is_agent"] != true {
		t.Error("expected is_agent=true")
	}
	if m["agent_id"] != "agent-001" {
		t.Errorf("agent_id=%v, want agent-001", m["agent_id"])
	}
	if m["section"] != "testing" {
		t.Errorf("section=%v, want testing", m["section"])
	}
	if m["status"] != "running" {
		t.Errorf("status=%v, want running", m["status"])
	}
	// Project must be surfaced from agent record so spawned agents can use
	// it verbatim for scratchpad_write/read calls — prevents scope drift
	// when orchestrator spawns with project=path and agent guesses basename.
	if m["project"] != "proj-a" {
		t.Errorf("project=%v, want proj-a (agent.Project should surface)", m["project"])
	}
}

func TestHandleWhoami_EmptySession(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "whoami", Params: map[string]any{}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["session_id"] != "" {
		t.Errorf("expected empty session_id, got %v", m["session_id"])
	}
	if m["is_agent"] != false {
		t.Error("expected is_agent=false")
	}
}

// --- formatCheckMessagesResult ---

func TestFormatCheckMessagesResult_WithMessages(t *testing.T) {
	raw := json.RawMessage(`{"has_dialog":true,"topic":"collab","messages":[{"sender":"abcdefghij","content":"hello world"}]}`)
	got := formatCheckMessagesResult(raw)
	if got == "" {
		t.Fatal("expected non-empty output")
	}
	if got != "\U0001f4e8 DIALOG [collab] abcdefgh: hello world\n" {
		t.Errorf("unexpected format: %q", got)
	}
}

// The sender label is truncated with a literal [:8]; a sender shorter than that
// panicked the whole formatter (YM-73).
func TestFormatCheckMessagesResult_ShortSender(t *testing.T) {
	raw := json.RawMessage(`{"has_dialog":true,"topic":"t","messages":[{"sender":"ab","content":"hi"}]}`)
	got := formatCheckMessagesResult(raw)
	if got != "\U0001f4e8 DIALOG [t] ab: hi\n" {
		t.Errorf("unexpected format: %q", got)
	}
}

// The sender label is truncated to 8 characters; counting bytes cut a multi-byte
// label apart and produced invalid UTF-8 in the text an agent reads.
func TestFormatCheckMessagesResult_MultiByteSenderKeepsWholeCharacters(t *testing.T) {
	raw := json.RawMessage(`{"has_dialog":true,"topic":"t","messages":[{"sender":"äöüßéèàçx","content":"hi"}]}`)
	got := formatCheckMessagesResult(raw)
	if !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 in %q", got)
	}
	if !strings.Contains(got, "äöüßéèàç") {
		t.Errorf("the label was cut mid-character: %q", got)
	}
}

func TestFormatCheckMessagesResult_NoDialog(t *testing.T) {
	raw := json.RawMessage(`{"has_dialog":false,"messages":[]}`)
	got := formatCheckMessagesResult(raw)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFormatCheckMessagesResult_EmptyMessages(t *testing.T) {
	raw := json.RawMessage(`{"has_dialog":true,"messages":[],"topic":"t"}`)
	got := formatCheckMessagesResult(raw)
	if got != "" {
		t.Errorf("expected empty for no messages, got %q", got)
	}
}

func TestFormatCheckMessagesResult_InvalidJSON(t *testing.T) {
	raw := json.RawMessage(`{invalid}`)
	got := formatCheckMessagesResult(raw)
	if got != "" {
		t.Errorf("expected empty for invalid JSON, got %q", got)
	}
}

// --- channel inbox ownership (YM-204) ---

// The proxy is the installation's channel consumer: it reads and marks for the
// session whose request it is serving and has no session of its own. Its
// connection is the peer the socket layer reports, so the named inbox stays open
// for it.
func TestCheckChannel_ProxyPeerReadsTheNamedSession(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage("opencode:ses_TARGET", "someone", "for the target", "command"); err != nil {
		t.Fatal(err)
	}
	stubProcCmdline(t, "/home/tester/.local/bin/yesmem\x00proxy\x00")

	resp := h.Handle(Request{Method: "check_channel", Params: map[string]any{
		"session_id": "opencode:ses_TARGET", "_peer_pid": 4242,
	}})
	if resp.Error != "" {
		t.Fatalf("the proxy was refused its own consumer path: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "for the target") {
		t.Errorf("proxy did not receive the message: %s", resp.Result)
	}
}

// A session_id is not an ownership proof. A caller that arrives on a connection
// and is not the proxy must not read a foreign inbox — and it must not be able to
// talk its way in either: naming the victim as its own identity used to be
// enough, because that value is one the caller chooses (review finding on
// YM-204).
func TestCheckChannel_ForeignInboxStaysClosed(t *testing.T) {
	requireProcTable(t)
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage("opencode:ses_VICTIM", "someone", "for the victim", "command"); err != nil {
		t.Fatal(err)
	}
	// The peer of this connection is not the proxy.
	stubProcCmdline(t, "/usr/bin/opencode\x00serve\x00")

	for name, extra := range map[string]map[string]any{
		"an identity it asserts itself":               {"_session_id": "opencode:ses_VICTIM"},
		"the source marker that makes itself trusted": {"_session_id": "opencode:ses_VICTIM", "_session_id_source": "env"},
		"its own unrelated identity":                  {"_session_id": "caller-session"},
		"nothing at all":                              {},
	} {
		params := map[string]any{"session_id": "opencode:ses_VICTIM", "_peer_pid": 4321}
		for k, v := range extra {
			params[k] = v
		}
		resp := h.Handle(Request{Method: "check_channel", Params: params})
		if resp.Error == "" {
			t.Errorf("%s read a foreign inbox: %s", name, resp.Result)
		}
		if strings.Contains(string(resp.Result), "for the victim") {
			t.Errorf("%s received the victim's message: %s", name, resp.Result)
		}
	}

	// A pid the caller sends itself is not a peer: JSON decoding makes every
	// number a float64, and only the int the socket layer writes counts.
	resp := h.Handle(Request{Method: "check_channel", Params: map[string]any{
		"session_id": "opencode:ses_VICTIM", "_peer_pid": float64(4321),
	}})
	if resp.Error != "" {
		t.Errorf("a caller-supplied pid must not count as a peer at all, and with no peer the in-process path stands: %s", resp.Error)
	}
}

// The peer pid has to be the transport's own value, not one a caller can send:
// the socket layer writes an int, while anything arriving through JSON decoding
// is a float64 and is ignored. A caller that names a pid of its choosing must
// not reach the peer branch in either direction.
func TestPeerPIDParam_OnlyTheSocketsIntCounts(t *testing.T) {
	if got := peerPIDParam(map[string]any{"_peer_pid": float64(4321)}); got != 0 {
		t.Errorf("a JSON number was read as a peer pid: %d", got)
	}
	if got := peerPIDParam(map[string]any{"_peer_pid": "4321"}); got != 0 {
		t.Errorf("a string was read as a peer pid: %d", got)
	}
	if got := peerPIDParam(map[string]any{"_peer_pid": 4321}); got != 4321 {
		t.Errorf("the socket layer's int form was not read: %d", got)
	}
	if got := peerPIDParam(map[string]any{}); got != 0 {
		t.Errorf("a request without a peer must read 0: %d", got)
	}
}

// The HTTP RPC pass-through carries no peer, so the channel endpoints cannot be
// held to a caller there. They exist for the proxy's socket calls; an HTTP
// bearer-token holder must not get a session's inbox through the public API
// (review finding on YM-204).
func TestHTTPAdapter_DoesNotServeTheChannelEndpoints(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage("opencode:ses_VICTIM", "someone", "for the victim", "command"); err != nil {
		t.Fatal(err)
	}
	adapter := &handlerAdapter{h: h}

	for _, method := range []string{"check_channel", "mark_channel_read", "check_broadcasts"} {
		resp := adapter.Handle(httpapi.RPCRequest{Method: method, Params: map[string]any{"session_id": "opencode:ses_VICTIM"}})
		if resp.Error == "" {
			t.Errorf("%s is served over HTTP and read a foreign inbox: %s", method, resp.Result)
		}
	}
	// broadcast writes, but its label is resolved from the caller's proven
	// identity — and that proof lives on the socket (YM-271).
	if resp := adapter.Handle(httpapi.RPCRequest{Method: "broadcast", Params: map[string]any{
		"project": "http-proj", "content": "forged", "sender": "opencode:ses_VICTIM",
	}}); resp.Error == "" {
		t.Errorf("broadcast is served over HTTP, where no peer can vouch for the sender: %s", resp.Result)
	}
	left, err := s.GetChannelMessages(s.ChannelAddresses("opencode:ses_VICTIM"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("the victim's message was consumed over HTTP: %d left", len(left))
	}
}

// The daemon's own callers pass no peer, and a platform without peer credentials
// never has one. That path keeps the old behaviour, which is deliberate: the
// guard closes the socket, not the daemon's internal calls.
func TestCheckChannel_InProcessCallerKeepsTheNamedSession(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage("opencode:ses_INTERNAL", "someone", "internal read", "command"); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "check_channel", Params: map[string]any{
		"session_id": "opencode:ses_INTERNAL",
	}})
	if resp.Error != "" {
		t.Fatalf("an in-process caller was refused: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "internal read") {
		t.Errorf("an in-process caller lost its inbox: %s", resp.Result)
	}
}

// mark_channel_read consumes the mail it names, so the same guard has to hold
// there: otherwise a foreign caller can silently swallow another session's
// messages.
func TestMarkChannelRead_ForeignInboxStaysClosed(t *testing.T) {
	requireProcTable(t)
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage("opencode:ses_VICTIM", "someone", "for the victim", "command"); err != nil {
		t.Fatal(err)
	}
	stubProcCmdline(t, "/usr/bin/opencode\x00serve\x00")

	resp := h.Handle(Request{Method: "mark_channel_read", Params: map[string]any{
		"session_id": "opencode:ses_VICTIM", "_peer_pid": 4321, "_session_id": "caller-session",
	}})
	if resp.Error == "" {
		t.Error("a foreign caller marked another session's inbox read")
	}
	left, err := s.GetChannelMessages(s.ChannelAddresses("opencode:ses_VICTIM"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("the victim's message was consumed: %d left", len(left))
	}
}

func TestHandleWhoami_IncludesModelFromProxyState(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.SetProxyState("session_model:sid-w-model", "claude-opus-4-7"); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "whoami", Params: map[string]any{"session_id": "sid-w-model"}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["model"] != "claude-opus-4-7" {
		t.Errorf("model=%v, want claude-opus-4-7", m["model"])
	}
}

func TestHandleWhoami_OmitsModelWhenProxyStateMissing(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.Handle(Request{Method: "whoami", Params: map[string]any{"session_id": "no-model-sess"}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if v, ok := m["model"]; ok && v != "" {
		t.Errorf("model should be empty/missing when proxy_state absent, got %v", v)
	}
}

// --- whoami prefix-strip (opencode/codex backend sessions) ---
// resolveSessionID returns "opencode:<id>" / "codex:<id>" (prefixed) when the
// caller came in via the OpenAI/proxy path. The DB stores bare ids in
// opencode_session_id / codex_session_id. The prefix is stripped at the
// storage-layer boundary (internal/storage/agents.go stripAgentPrefix) inside
// AgentGetAnyBySession; whoami passes sessionID verbatim and keeps the
// prefixed value in the response to preserve the caller contract.

func TestHandleWhoami_StripsOpenCodePrefix(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-oc-prefix", Project: "proj-oc", Section: "sec-oc",
		SessionID: "synthetic-oc", OpencodeSessionID: "ses_oc_real_1",
		Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "whoami", Params: map[string]any{
		"session_id":    "opencode:ses_oc_real_1",
		"_source_agent": "opencode",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["is_agent"] != true {
		t.Errorf("is_agent=%v, want true (opencode prefix should still match)", m["is_agent"])
	}
	if m["agent_id"] != "agent-oc-prefix" {
		t.Errorf("agent_id=%v, want agent-oc-prefix", m["agent_id"])
	}
	if m["project"] != "proj-oc" {
		t.Errorf("project=%v, want proj-oc", m["project"])
	}
	if m["session_id"] != "opencode:ses_oc_real_1" {
		t.Errorf("session_id=%v, want prefixed value preserved in response", m["session_id"])
	}
}

func TestHandleWhoami_StripsCodexPrefix(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-cx-prefix", Project: "proj-cx", Section: "sec-cx",
		SessionID: "synthetic-cx", CodexSessionID: "ses_cx_real_1",
		Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "whoami", Params: map[string]any{
		"session_id":    "codex:ses_cx_real_1",
		"_source_agent": "codex",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	m := resultMap(t, resp)
	if m["is_agent"] != true {
		t.Errorf("is_agent=%v, want true (codex prefix should still match)", m["is_agent"])
	}
	if m["agent_id"] != "agent-cx-prefix" {
		t.Errorf("agent_id=%v, want agent-cx-prefix", m["agent_id"])
	}
	if m["project"] != "proj-cx" {
		t.Errorf("project=%v, want proj-cx", m["project"])
	}
}

// TestHandleRegisterPID_ReRegisterDisplacesOldSession verifies that
// re-registering the same PID with a new session ID removes the old
// mapping. Without this, resolveSessionID iterates pidMap and can
// return a stale session from a previous PID→session registration.
func TestHandleRegisterPID_ReRegisterDisplacesOldSession(t *testing.T) {
	h, _ := mustHandler(t)

	// Register session A with PID 100.
	h.Handle(Request{Method: "register_pid", Params: map[string]any{
		"session_id": "session-A", "pid": float64(100),
	}})

	// Verify session A is mapped.
	h.pidMapMu.Lock()
	_, hasA := h.pidMap["session-A"]
	pidForA := 0
	for sid, p := range h.pidMap {
		if sid == "session-A" {
			pidForA = p
		}
	}
	h.pidMapMu.Unlock()
	if !hasA || pidForA != 100 {
		t.Fatalf("expected pidMap[session-A]=100, got has=%v pid=%d", hasA, pidForA)
	}

	// Re-register same PID 100 with session B.
	h.Handle(Request{Method: "register_pid", Params: map[string]any{
		"session_id": "session-B", "pid": float64(100),
	}})

	h.pidMapMu.Lock()
	_, hasA = h.pidMap["session-A"]
	pidForB, hasB := h.pidMap["session-B"]
	h.pidMapMu.Unlock()
	if hasA {
		t.Error("session-A still in pidMap after re-registration with same PID — must be displaced")
	}
	if !hasB || pidForB != 100 {
		t.Errorf("session-B not properly registered: has=%v pid=%d", hasB, pidForB)
	}
}

// TestHandleRegisterPID_ReRegisterDisplacesAllOldSessions verifies that
// ALL stale same-PID entries are removed (not just the first). Multiple
// stale entries can accumulate if PID files persist across daemon restarts
// or through the DB fallback path. Manual pidMap population simulates this.
func TestHandleRegisterPID_ReRegisterDisplacesAllOldSessions(t *testing.T) {
	h, _ := mustHandler(t)

	// Manually inject multiple stale session→PID entries (simulates race
	// or PID-file-based recovery that created duplicates).
	h.pidMapMu.Lock()
	h.pidMap["session-A"] = 100
	h.pidMap["session-B"] = 100
	h.pidMap["session-C"] = 100
	h.pidMap["session-D"] = 200 // different PID, should survive
	h.pidMapMu.Unlock()

	// Verify initial state: 3 stale entries for PID 100, 1 for PID 200.
	h.pidMapMu.Lock()
	count100 := 0
	for _, p := range h.pidMap {
		if p == 100 {
			count100++
		}
	}
	h.pidMapMu.Unlock()
	if count100 != 3 {
		t.Fatalf("expected 3 pidMap entries for PID 100, got %d", count100)
	}

	// Register PID 100 with session X — must remove A, B, C.
	h.Handle(Request{Method: "register_pid", Params: map[string]any{
		"session_id": "session-X", "pid": float64(100),
	}})

	h.pidMapMu.Lock()
	remaining100 := 0
	has200 := false
	for sid, p := range h.pidMap {
		if p == 100 {
			remaining100++
			if sid != "session-X" {
				t.Errorf("stale entry %q survived cleanup (PID=100)", sid)
			}
		}
		if p == 200 {
			has200 = true
		}
	}
	h.pidMapMu.Unlock()
	if remaining100 != 1 {
		t.Errorf("expected 1 remaining entry for PID 100, got %d", remaining100)
	}
	if !has200 {
		t.Error("unrelated PID 200 was incorrectly removed")
	}
}

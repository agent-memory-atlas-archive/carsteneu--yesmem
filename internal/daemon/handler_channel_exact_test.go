package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

const (
	testClaudeA = "a542b034-d69d-4e10-9553-a549eefa4d77"
	testClaudeB = "b1234567-aaaa-bbbb-cccc-dddddddddddd"
	testCodexA  = "codex:7f2c1b44-0000-4111-8222-333344445555"
	testCodexB  = "codex:8a3d2c55-1111-4222-8333-444455556666"
	testOcA     = "opencode:ses_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	testOcB     = "opencode:ses_bbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func sendTo(h *Handler, target, content, sender string) Response {
	return h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": target, "content": content, "sender": sender,
	}})
}

// checkChannel returns the messages the session would receive right now.
func checkChannel(t *testing.T, h *Handler, sessionID string) []map[string]any {
	t.Helper()
	resp := h.Handle(Request{Method: "check_channel", Params: map[string]any{"session_id": sessionID}})
	if resp.Error != "" {
		t.Fatalf("check_channel(%s): %s", sessionID, resp.Error)
	}
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("check_channel(%s) result %s: %v", sessionID, string(resp.Result), err)
	}
	return out.Messages
}

func responseTarget(t *testing.T, resp Response) string {
	t.Helper()
	if resp.Error != "" {
		t.Fatalf("send_to error: %s", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("send_to result %s: %v", string(resp.Result), err)
	}
	target, _ := result["target"].(string)
	return target
}

// Two sessions of the same harness must never see each other's messages —
// including opencode, whose 8-character prefix is the same for all sessions.
func TestChannelDelivery_SameHarnessSessionsStaySeparate(t *testing.T) {
	pairs := [][2]string{
		{testClaudeA, testClaudeB},
		{testCodexA, testCodexB},
		{testOcA, testOcB},
		{"orchestrator", "telegram-bridge-48631"},
	}
	for _, pair := range pairs {
		a, b := pair[0], pair[1]
		t.Run(a, func(t *testing.T) {
			h, _ := mustHandler(t)

			if resp := sendTo(h, a, "for A", "some-sender"); resp.Error != "" {
				t.Fatal(resp.Error)
			}

			if got := checkChannel(t, h, b); len(got) != 0 {
				t.Errorf("%s received %d message(s) addressed to %s", b, len(got), a)
			}
			got := checkChannel(t, h, a)
			if len(got) != 1 {
				t.Fatalf("%s received %d message(s), want 1", a, len(got))
			}
			if got[0]["content"] != "for A\n" {
				t.Errorf("content = %v, want %q", got[0]["content"], "for A\n")
			}
		})
	}
}

// A message addressed to one of an agent's addresses is readable under all of them.
func TestChannelDelivery_AgentReachableByAllItsAddresses(t *testing.T) {
	h, s := mustHandler(t)
	const oc = "ses_cccccccccccccccccccccccccc"
	const synthetic = "11112222-3333-4444-5555-666677778888"
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-r1", Project: "p", Section: "sec",
		SessionID: synthetic, OpencodeSessionID: oc, Status: "running",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	// Addressed by the synthetic daemon uuid …
	if got := responseTarget(t, sendTo(h, synthetic, "via synthetic", "sender-1")); got != "opencode:"+oc {
		t.Errorf("target = %q, want the canonical backend address %q", got, "opencode:"+oc)
	}
	// … and by a short form of the backend id.
	if resp := sendTo(h, oc[:10], "via prefix", "sender-1"); resp.Error != "" {
		t.Fatal(resp.Error)
	}

	for _, readAs := range []string{synthetic, "opencode:" + oc, oc} {
		if got := checkChannel(t, h, readAs); len(got) != 2 {
			t.Errorf("reading as %q saw %d message(s), want 2", readAs, len(got))
		}
	}
}

func TestHandleSendTo_AmbiguousTargetIsRefused(t *testing.T) {
	h, s := mustHandler(t)
	for _, a := range []storage.Agent{
		{ID: "agent-a1", Project: "p", Section: "s1", SessionID: "ebfd8cca-1111-4111-8111-111111111111", Status: "running"},
		{ID: "agent-a2", Project: "p", Section: "s2", SessionID: "ebfd8cca-2222-4222-8222-222222222222", Status: "running"},
	} {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate: %v", err)
		}
	}

	resp := sendTo(h, "ebfd8cca", "hello", "sender-1")
	if resp.Error == "" {
		t.Fatal("expected an error for an ambiguous target")
	}
	for _, want := range []string{"ambiguous", "agent-a1", "agent-a2"} {
		if !strings.Contains(resp.Error, want) {
			t.Errorf("error %q should mention %q", resp.Error, want)
		}
	}
	// Nothing may be stored for a refused target.
	for _, id := range []string{"ebfd8cca-1111-4111-8111-111111111111", "ebfd8cca-2222-4222-8222-222222222222"} {
		if got := checkChannel(t, h, id); len(got) != 0 {
			t.Errorf("refused send still delivered %d message(s) to %s", len(got), id)
		}
	}
}

func TestHandleSendTo_UnknownShortTargetIsRefused(t *testing.T) {
	h, _ := mustHandler(t)

	resp := sendTo(h, "deadbeef", "hello", "sender-1")
	if resp.Error == "" {
		t.Fatal("expected an error for a short target without a known agent")
	}
	if !strings.Contains(resp.Error, "no known agent") {
		t.Errorf("error = %q, want it to name the missing agent", resp.Error)
	}
}

// Complete addresses are exact addresses, not fuzzy ones: they pass through
// unchanged even when no agent row exists (named channels, plain sessions).
func TestHandleSendTo_CompleteAddressAndNamedChannelPassThrough(t *testing.T) {
	h, _ := mustHandler(t)

	cases := []struct {
		target string
		want   string
	}{
		{"orchestrator", "orchestrator"},
		{"telegram-bridge-48631", "telegram-bridge-48631"},
		{testClaudeA, testClaudeA},
		{"opencode:ses_0123456789abcdefghijklmnop", "opencode:ses_0123456789abcdefghijklmnop"},
		{"ses_0123456789abcdefghijklmnop", "opencode:ses_0123456789abcdefghijklmnop"},
	}
	for _, c := range cases {
		if got := responseTarget(t, sendTo(h, c.target, "hi", "sender-1")); got != c.want {
			t.Errorf("send_to(%q) reported target %q, want %q", c.target, got, c.want)
		}
	}
}

// The implicit ack acknowledges the sender's own inbox only.
func TestHandleSendTo_ImplicitAckStaysWithTheSender(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage(testOcA, "someone", "unread for A", "command"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendChannelMessage(testOcB, "someone", "unread for B", "command"); err != nil {
		t.Fatal(err)
	}

	// A real caller: the MCP layer always injects the caller's own _session_id.
	// An explicit id-like sender is only acknowledged when it is that caller.
	if resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "orchestrator", "content": "hello", "sender": testOcB, "_session_id": testOcB,
	}}); resp.Error != "" {
		t.Fatal(resp.Error)
	}

	if got := checkChannel(t, h, testOcA); len(got) != 1 {
		t.Errorf("A's message was acknowledged by B's send: %d left, want 1", len(got))
	}
	if got := checkChannel(t, h, testOcB); len(got) != 0 {
		t.Errorf("B's own inbox should be acknowledged: %d left, want 0", len(got))
	}
}

func TestHandleMarkChannelRead_ScopedToSession(t *testing.T) {
	h, s := mustHandler(t)
	s.SendChannelMessage(testOcA, "someone", "for A", "command")
	s.SendChannelMessage(testOcB, "someone", "for B", "command")

	resp := h.Handle(Request{Method: "mark_channel_read", Params: map[string]any{"session_id": testOcA}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}

	if got := checkChannel(t, h, testOcA); len(got) != 0 {
		t.Errorf("A still has %d message(s) after mark_channel_read", len(got))
	}
	if got := checkChannel(t, h, testOcB); len(got) != 1 {
		t.Errorf("marking A read touched B: %d message(s) left, want 1", len(got))
	}
}

func TestHandleCheckChannel_UnknownSessionIsEmpty(t *testing.T) {
	h, _ := mustHandler(t)
	if got := checkChannel(t, h, "opencode:ses_unknown"); len(got) != 0 {
		t.Errorf("unknown session saw %d message(s)", len(got))
	}
}

func TestHandleSendTo_TruncatedNamespaceRefused(t *testing.T) {
	h, _ := mustHandler(t)

	// The 8-character stubs the old send_to advertised must not become silent
	// dead addresses.
	for _, token := range []string{"opencode", "codex", "claude"} {
		resp := sendTo(h, token, "hi", "sender-1")
		if resp.Error == "" {
			t.Errorf("send_to(%q) was accepted — a truncated namespace is a black hole", token)
			continue
		}
		if !strings.Contains(resp.Error, "truncated namespace") {
			t.Errorf("send_to(%q) error = %q, want a truncated-namespace message", token, resp.Error)
		}
	}
}

func TestHandleCheckChannel_ByAgentID(t *testing.T) {
	h, s := mustHandler(t)
	const oc = "ses_qqqqqqqqqqqqqqqqqqqqqqqqqq"
	const synthetic = "55556666-7777-4888-8999-000011112222"
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-byid", Project: "p", Section: "sec",
		SessionID: synthetic, OpencodeSessionID: oc, Status: "running",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	if resp := sendTo(h, "opencode:"+oc, "hello", "sender-1"); resp.Error != "" {
		t.Fatal(resp.Error)
	}

	if got := checkChannel(t, h, "agent-byid"); len(got) != 1 {
		t.Errorf("reading by agent id saw %d message(s), want 1", len(got))
	}
}

func TestHandleSendTo_SharedBackendAddressResolves(t *testing.T) {
	h, s := mustHandler(t)
	const oc = "ses_rrrrrrrrrrrrrrrrrrrrrrrrrr"
	for _, a := range []storage.Agent{
		{ID: "agent-shared-old", Project: "p", Section: "s1", SessionID: "aaaa9999-0000-4000-8000-000000000009", OpencodeSessionID: oc, Status: "stopped", CreatedAt: "2026-01-01T00:00:00+01:00"},
		{ID: "agent-shared-new", Project: "p", Section: "s2", SessionID: "bbbb9999-0000-4000-8000-00000000000b", OpencodeSessionID: oc, Status: "running", CreatedAt: "2026-02-01T00:00:00+01:00"},
	} {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate(%s): %v", a.ID, err)
		}
	}

	// A restarted agent must stay addressable by its backend address.
	if got := responseTarget(t, sendTo(h, "opencode:"+oc, "hi", "sender-1")); got != "opencode:"+oc {
		t.Errorf("target = %q, want %q", got, "opencode:"+oc)
	}
}

// TestSendTo_UnboundOpencodeAgentWarns: a message to an opencode agent whose
// session was never captured is stored under the agent's synthetic session UUID,
// which its proxy never polls. The send is not an error — the bind may still
// succeed later — but treating it as delivered would hide a lost message, so the
// response carries the warning.
func TestSendTo_UnboundOpencodeAgentWarns(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-unbound", Project: "proj", Section: "sec",
		SessionID: "3f1a2b3c-0000-4000-8000-000000000001",
		Status:    "running", Backend: "opencode",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-bound", Project: "proj", Section: "sec2",
		SessionID: "3f1a2b3c-0000-4000-8000-000000000002",
		Status:    "running", Backend: "opencode",
		OpencodeSessionID: "ses_bound0000000000000000000000",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	m := resultMap(t, sendTo(h, "agent-unbound", "hi", "orchestrator"))
	if m["warning"] != sendTargetUnboundWarning {
		t.Errorf("warning = %v, want %q", m["warning"], sendTargetUnboundWarning)
	}

	bound := resultMap(t, sendTo(h, "agent-bound", "hi", "orchestrator"))
	if _, ok := bound["warning"]; ok {
		t.Errorf("a bound agent must not warn, got %v", bound["warning"])
	}
}

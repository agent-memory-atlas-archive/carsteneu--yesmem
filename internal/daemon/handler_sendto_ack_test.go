package daemon

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// The implicit ACK in handleSendTo marks the sender's own incoming messages as
// read. It may only do that for an identity that is proven to be the caller:
// an explicitly passed id-like sender is a free parameter, and the global
// active-session fallback names whichever session wrote last.

func unreadFor(t *testing.T, h *Handler, addr string) int {
	t.Helper()
	_, s := h, h.store
	msgs, err := s.GetChannelMessages([]string{addr})
	if err != nil {
		t.Fatalf("GetChannelMessages(%s): %v", addr, err)
	}
	return len(msgs)
}

func TestSendTo_ExplicitForeignSenderDoesNotAck(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage(testOcB, "someone", "for B", "command"); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "orchestrator", "content": "hi", "sender": testOcB,
		"_session_id": testOcA,
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if n := unreadFor(t, h, testOcB); n != 1 {
		t.Errorf("a caller claiming a foreign id-like sender drained its inbox: unread=%d, want 1", n)
	}
}

func TestSendTo_ExplicitSenderMatchingCallerAcks(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage(testOcA, "someone", "for A", "command"); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "orchestrator", "content": "hi", "sender": testOcA,
		"_session_id": testOcA,
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if n := unreadFor(t, h, testOcA); n != 0 {
		t.Errorf("the caller's own inbox was not acknowledged: unread=%d, want 0", n)
	}
}

func TestSendTo_DerivedProvenCallerAcks(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage(testOcA, "someone", "for A", "command"); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "orchestrator", "content": "hi", "_session_id": testOcA,
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if n := unreadFor(t, h, testOcA); n != 0 {
		t.Errorf("the proven caller's inbox was not acknowledged: unread=%d, want 0", n)
	}
}

func TestSendTo_PaddedForeignSenderDoesNotAck(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage(testOcB, "someone", "for B", "command"); err != nil {
		t.Fatal(err)
	}
	for _, pad := range []string{" ", "\t", "\n"} {
		resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
			"target": "orchestrator", "content": "hi", "sender": pad + testOcB,
			"_session_id": testOcA,
		}})
		if resp.Error != "" {
			t.Fatal(resp.Error)
		}
		if n := unreadFor(t, h, testOcB); n != 1 {
			t.Fatalf("sender %q drained B's inbox: unread=%d, want 1", pad+testOcB, n)
		}
	}
}

func TestSendTo_UnderscoreSenderIsNoProof(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage(testOcB, "someone", "for B", "command"); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "orchestrator", "content": "hi", "sender": testOcB, "_sender": testOcB,
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if n := unreadFor(t, h, testOcB); n != 1 {
		t.Errorf("a self-declared _sender counted as proof: unread=%d, want 1", n)
	}
}

func TestSendTo_SyntheticAliasOfProvenCallerAcks(t *testing.T) {
	h, s := mustHandler(t)
	const synthetic = "5f0c2a91-1111-4222-8333-444455556666"
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-alias", Project: "/p", Section: "alias", SessionID: synthetic,
		Status: "running", Backend: "opencode", OpencodeSessionID: strings.TrimPrefix(testOcA, "opencode:"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendChannelMessage(testOcA, "someone", "for A", "command"); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "orchestrator", "content": "hi", "sender": synthetic, "_session_id": testOcA,
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if n := unreadFor(t, h, testOcA); n != 0 {
		t.Errorf("the caller's synthetic alias was not acknowledged: unread=%d, want 0", n)
	}
}

func TestSendTo_NamedChannelSenderKeepsLegacyAck(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage("telegram-bridge-48631", "someone", "for the bridge", "command"); err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "orchestrator", "content": "hi", "sender": "telegram-bridge-48631",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if n := unreadFor(t, h, "telegram-bridge-48631"); n != 0 {
		t.Errorf("a named channel lost its implicit ack: unread=%d, want 0", n)
	}
}

// The ACK-on-ACK check pairs sender and target, so it may only run with an
// identity the caller can be held to. With the global active-session fallback as
// the sender, a self-addressed status row (target == sender; live 2026-10-16: 37
// of 1080 status rows) made the check drop every following message before it was
// stored — daemon status reports to the orchestrator among them. YM-194.
func TestSendTo_GuessedSenderDoesNotDropStatusMessages(t *testing.T) {
	h, s := mustHandler(t)

	// The counterparty of the would-be loop: a status row addressing its own
	// sender, exactly the shape that made the check fire.
	if _, err := s.SendChannelMessage(testOcA, testOcA, "earlier status", "status"); err != nil {
		t.Fatal(err)
	}
	// The request carries no identity, so the sender resolves to the global
	// active-session guess — which names the target here.
	h.activeSessionMu.Lock()
	h.activeSessionID = testOcA
	h.activeSessionMu.Unlock()

	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": testOcA, "content": "status report", "msg_type": "status",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if m := resultMap(t, resp); m["ack_dropped"] == true {
		t.Error("a guessed sender vetoed the message — it was never stored")
	}
	msgs, err := s.GetChannelMessages([]string{testOcA})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("stored messages = %d, want 2 (the seeded row plus the new report)", len(msgs))
	}
}

// The implicit ack consumes the caller's own pending mail — but not a message the
// heartbeat only pinged. That one has promised content ("content arrives via
// system prompt") which has not been injected yet, and the ack would make it
// unreachable for the proxy (review finding on YM-58: two consumers, two
// meanings).
func TestSendTo_ImplicitAckDoesNotConsumeAPingedMessage(t *testing.T) {
	h, s := mustHandler(t)
	id, err := s.SendChannelMessage(testOcA, "someone", "for A", "command")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMessageNotified(id); err != nil {
		t.Fatalf("MarkMessageNotified: %v", err)
	}

	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "orchestrator", "content": "hi", "_session_id": testOcA,
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if n := unreadFor(t, h, testOcA); n != 1 {
		t.Errorf("the implicit ack consumed a pinged message whose content was never injected: unread=%d, want 1", n)
	}
}

func TestSendTo_GlobalFallbackSenderDoesNotAck(t *testing.T) {
	h, s := mustHandler(t)
	if _, err := s.SendChannelMessage(testOcB, "someone", "for B", "command"); err != nil {
		t.Fatal(err)
	}
	h.activeSessionMu.Lock()
	h.activeSessionID = testOcB
	h.activeSessionMu.Unlock()
	resp := h.Handle(Request{Method: "send_to", Params: map[string]any{
		"target": "orchestrator", "content": "hi",
	}})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if n := unreadFor(t, h, testOcB); n != 1 {
		t.Errorf("an unidentified caller drained the last active session's inbox: unread=%d, want 1", n)
	}
}

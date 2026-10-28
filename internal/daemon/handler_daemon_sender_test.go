package daemon

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// setActiveSession makes id the session resolveSessionID falls back to — the
// "last writer" the daemon's own notices used to be attributed to.
func setActiveSession(h *Handler, id string) {
	h.activeSessionMu.Lock()
	h.activeSessionID = id
	h.activeSessionMu.Unlock()
}

// A daemon notice must name the daemon, not whichever session wrote last.
func TestNotifyOrchestrator_CarriesDaemonSender(t *testing.T) {
	h, _ := mustHandler(t)
	setActiveSession(h, testOcB)

	agent := storage.Agent{ID: "agent-notice", Project: "p", Section: "yesloop-notice", CallerSession: testOcA}
	h.notifyOrchestrator(agent, "DEAD_AGENT: agent-notice idle")

	msgs := checkChannel(t, h, testOcA)
	if len(msgs) != 1 {
		t.Fatalf("orchestrator received %d message(s), want 1", len(msgs))
	}
	sender, _ := msgs[0]["sender"].(string)
	if sender == testOcB {
		t.Fatalf("notice attributed to the last active session %q", sender)
	}
	if !strings.HasPrefix(sender, "daemon:") {
		t.Errorf("sender = %q, want a daemon: address", sender)
	}
}

// The damage of a missing sender: handleSendTo runs the implicit ACK for it, and
// that ACK drains the inbox of the session the sender resolved to.
func TestNotifyOrchestrator_LeavesForeignInboxUnread(t *testing.T) {
	h, s := mustHandler(t)
	setActiveSession(h, testOcB)
	if _, err := s.SendChannelMessage(testOcB, "someone", "waiting for B", "command"); err != nil {
		t.Fatal(err)
	}

	agent := storage.Agent{ID: "agent-notice", Project: "p", Section: "yesloop-notice", CallerSession: testOcA}
	h.notifyOrchestrator(agent, "DEAD_AGENT: agent-notice idle")

	if got := checkChannel(t, h, testOcB); len(got) != 1 {
		t.Errorf("the daemon notice acknowledged B's inbox: %d message(s) left, want 1", len(got))
	}
}

// A daemon address names no session, so there is no inbox to acknowledge.
func TestHandleSendTo_DaemonSenderDoesNotAck(t *testing.T) {
	h, s := mustHandler(t)
	daemon := daemonSender("yesloop")
	if _, err := s.SendChannelMessage(daemon, "someone", "for the daemon", "command"); err != nil {
		t.Fatal(err)
	}

	if resp := sendTo(h, "orchestrator", "idle tick", daemon); resp.Error != "" {
		t.Fatal(resp.Error)
	}

	if got := checkChannel(t, h, daemon); len(got) != 1 {
		t.Errorf("a daemon sender acknowledged its own address: %d message(s) left, want 1", len(got))
	}
}

// The guard must test the form the ack would actually use: "claude:daemon:x" is
// stored as "daemon:x", so an end-anchored prefix test would let it through.
func TestHandleSendTo_DaemonSenderGuardUsesCanonicalForm(t *testing.T) {
	h, s := mustHandler(t)
	daemon := daemonSender("yesloop")
	if _, err := s.SendChannelMessage(daemon, "someone", "for the daemon", "command"); err != nil {
		t.Fatal(err)
	}

	if resp := sendTo(h, "orchestrator", "idle tick", "claude:"+daemon); resp.Error != "" {
		t.Fatal(resp.Error)
	}

	if got := checkChannel(t, h, daemon); len(got) != 1 {
		t.Errorf("a claude:-prefixed daemon sender acknowledged the daemon address: %d message(s) left, want 1", len(got))
	}
}

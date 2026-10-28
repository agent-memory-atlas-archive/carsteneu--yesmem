package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// relayAgentMessages used to query with agent.SessionID only, so a message
// addressed to an agent's backend id (opencode/codex) was never pinged.
func TestRelayAgentMessages_PingsForBackendAddressedMessage(t *testing.T) {
	h, s := mustHandler(t)
	const oc = "ses_dddddddddddddddddddddddddd"
	const synthetic = "22223333-4444-5555-6666-777788889999"
	sockPath := los24dAgentSock(t, h, "agent-hb-1")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-hb-1", Project: "p", Section: "sec",
		SessionID: synthetic, OpencodeSessionID: oc,
		// A foreign column proves the ping follows the derived path (YM-338).
		SockPath: filepath.Join(shortSockDir(t), "hb-victim.sock"), Status: "running",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	if _, err := s.SendChannelMessage("opencode:"+oc, "sender-1", "hello", "command"); err != nil {
		t.Fatal(err)
	}

	h.relayAgentMessages()

	waitForConnections(t, rec, 1)

	left, err := s.GetUnnotifiedMessages(s.ChannelAddresses(synthetic))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("message was not marked as notified: %d left", len(left))
	}
}

// The channel tables grew without bound; the heartbeat owns the retention run.
func TestPruneChannelHistory_DropsWhatNoReaderCanReach(t *testing.T) {
	h, s := mustHandler(t)

	old, err := s.SendBroadcast("s", "/proj", "old broadcast")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.SendBroadcast("s", "/proj", "fresh broadcast")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.SendChannelMessage("opencode:ses_prune_pending", "s", "waiting", "command")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE agent_broadcasts SET created_at = datetime('now','localtime','-48 hours') WHERE id = ?`, old); err != nil {
		t.Fatalf("backdate broadcast: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE agent_messages SET created_at = datetime('now','localtime','-48 hours') WHERE id = ?`, pending); err != nil {
		t.Fatalf("backdate message: %v", err)
	}

	h.pruneChannelHistory()

	rows := func(table string, id int64) int {
		t.Helper()
		var n int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if rows("agent_broadcasts", old) != 0 {
		t.Error("a broadcast past its read window survived the heartbeat prune")
	}
	if rows("agent_broadcasts", fresh) != 1 {
		t.Error("a fresh broadcast was pruned")
	}
	if rows("agent_messages", pending) != 1 {
		t.Error("an undelivered message was pruned — its reader would never see it")
	}
}

func TestRelayAgentMessages_UndeliverableMessageNotifiesSender(t *testing.T) {
	h, s := mustHandler(t)
	const synthetic = "77778888-9999-4000-8000-222233334444"
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-dead", Project: "p", Section: "sec",
		SessionID: synthetic, SockPath: "/nonexistent/dead.sock", Status: "running",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if _, err := s.SendChannelMessage(synthetic, "orchestrator", "never arrives", "command"); err != nil {
		t.Fatal(err)
	}

	// The inject socket never accepts: after 5 attempts the message is marked
	// failed and the sender must learn about it instead of being dropped silently.
	for i := 0; i < 5; i++ {
		h.relayAgentMessages()
	}

	got := checkChannel(t, h, "orchestrator")
	if len(got) != 1 {
		t.Fatalf("sender saw %d notice(s), want 1 DELIVERY_FAILED notice", len(got))
	}
	content, _ := got[0]["content"].(string)
	if !strings.Contains(content, "DELIVERY_FAILED") {
		t.Errorf("notice = %q, want a DELIVERY_FAILED notice", content)
	}
}

// A daemon sender has no inbox, so it can neither receive the DELIVERY_FAILED
// notice nor be re-notified — the row under its address is the last message on
// that address, which makes IsAckOnAck drop every repeat. It is logged instead.
func TestRelayAgentMessages_DaemonSenderIsNotNotified(t *testing.T) {
	h, s := mustHandler(t)
	const synthetic = "99990000-1111-4222-8333-444455556666"
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-dead-daemon", Project: "p", Section: "sec",
		SessionID: synthetic, SockPath: "/nonexistent/dead.sock", Status: "running",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	daemon := daemonSender("yesloop")
	if _, err := s.SendChannelMessage(synthetic, daemon, "never arrives", "status"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 7; i++ {
		h.relayAgentMessages()
	}

	if got := checkChannel(t, h, daemon); len(got) != 0 {
		t.Errorf("the daemon address received %d notice(s), want 0 — it has no inbox", len(got))
	}
}

func TestRelayAgentMessages_SkipsCodexAgents(t *testing.T) {
	h, s := mustHandler(t)
	const cx = "7f2c1b44-0000-4111-8222-333344445555"
	sockPath := filepath.Join(shortSockDir(t), "cx.sock")
	rec := startFakeInjectSocket(t, sockPath+".inject")
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-cx", Project: "p", Section: "sc", Backend: "codex",
		SessionID: "66667777-8888-4999-8000-111122223333", CodexSessionID: cx,
		SockPath: sockPath, Status: "running",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if _, err := s.SendChannelMessage("codex:"+cx, "sender-1", "for codex", "command"); err != nil {
		t.Fatal(err)
	}

	h.relayAgentMessages()

	// No consumer exists for codex channel content, so a ping would burn a turn
	// and hide the message behind read=1 forever.
	awaitNoConnection(t, rec)
	msgs, err := s.GetUnnotifiedMessages(s.ChannelAddresses("codex:" + cx))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("codex message must stay unnotified, got %d", len(msgs))
	}
}

func TestRelayAgentMessages_OnlyAddressedAgentIsPinged(t *testing.T) {
	h, s := mustHandler(t)
	const ocA = "ses_eeeeeeeeeeeeeeeeeeeeeeeeee"
	const ocB = "ses_ffffffffffffffffffffffffff"
	dir := shortSockDir(t)
	h.dataDir = dir
	sockA := filepath.Join(dir, "agent-hb-a.sock")
	sockB := filepath.Join(dir, "agent-hb-b.sock")
	recA := startFakeInjectSocket(t, sockA+".inject")
	recB := startFakeInjectSocket(t, sockB+".inject")
	for _, a := range []storage.Agent{
		// Foreign columns: only the derived path may see the ping (YM-338).
		{ID: "agent-hb-a", Project: "p", Section: "sa", SessionID: "33334444-5555-6666-7777-888899990000", OpencodeSessionID: ocA, SockPath: filepath.Join(shortSockDir(t), "a-victim.sock"), Status: "running"},
		{ID: "agent-hb-b", Project: "p", Section: "sb", SessionID: "44445555-6666-7777-8888-999900001111", OpencodeSessionID: ocB, SockPath: filepath.Join(shortSockDir(t), "b-victim.sock"), Status: "running"},
	} {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate(%s): %v", a.ID, err)
		}
	}

	if _, err := s.SendChannelMessage("opencode:"+ocB, "sender-1", "for B", "command"); err != nil {
		t.Fatal(err)
	}

	h.relayAgentMessages()

	waitForConnections(t, recB, 1)
	awaitNoConnection(t, recA)
}

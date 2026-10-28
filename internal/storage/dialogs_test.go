package storage

import (
	"fmt"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

func TestStartDialogAndGet(t *testing.T) {
	s := mustOpen(t)

	id, err := s.StartDialog("session-a", "session-b", "Ingest-Pipeline")
	if err != nil {
		t.Fatalf("StartDialog: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero dialog ID")
	}

	d, err := s.GetDialog(id)
	if err != nil {
		t.Fatalf("GetDialog: %v", err)
	}
	if d == nil {
		t.Fatal("expected dialog, got nil")
	}
	if d.Initiator != "session-a" || d.Partner != "session-b" {
		t.Errorf("wrong sessions: %s / %s", d.Initiator, d.Partner)
	}
	if d.Topic != "Ingest-Pipeline" {
		t.Errorf("wrong topic: %s", d.Topic)
	}
	if d.Status != "pending" {
		t.Errorf("expected pending, got %s", d.Status)
	}
}

func TestGetActiveDialogForSession(t *testing.T) {
	s := mustOpen(t)

	id, _ := s.StartDialog("session-a", "session-b", "test")
	s.UpdateDialogStatus(id, "active")

	// Both sides should find it
	for _, sid := range []string{"session-a", "session-b"} {
		d, err := s.GetActiveDialogForSession(sid)
		if err != nil {
			t.Fatalf("GetActiveDialogForSession(%s): %v", sid, err)
		}
		if d == nil {
			t.Fatalf("expected dialog for %s, got nil", sid)
		}
		if d.ID != id {
			t.Errorf("wrong dialog ID for %s: got %d, want %d", sid, d.ID, id)
		}
	}

	// Unrelated session should not find it
	d, _ := s.GetActiveDialogForSession("session-c")
	if d != nil {
		t.Error("session-c should not find dialog")
	}
}

func TestSendAndReceiveMessages(t *testing.T) {
	s := mustOpen(t)

	id, _ := s.StartDialog("a", "b", "test")

	// A sends 2 messages
	s.SendDialogMessage(id, "a", "hello from A")
	s.SendDialogMessage(id, "a", "second message")

	// B should see both
	msgs, err := s.GetUnreadMessages(id, "b")
	if err != nil {
		t.Fatalf("GetUnreadMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "hello from A" {
		t.Errorf("wrong content: %s", msgs[0].Content)
	}

	// A should NOT see own messages as unread
	ownMsgs, _ := s.GetUnreadMessages(id, "a")
	if len(ownMsgs) != 0 {
		t.Errorf("sender should not see own messages, got %d", len(ownMsgs))
	}
}

func TestMarkRead(t *testing.T) {
	s := mustOpen(t)

	id, _ := s.StartDialog("a", "b", "test")
	s.SendDialogMessage(id, "a", "msg1")
	s.SendDialogMessage(id, "a", "msg2")

	// Mark as read for B
	err := s.MarkMessagesRead(id, "b")
	if err != nil {
		t.Fatalf("MarkMessagesRead: %v", err)
	}

	// Should be empty now
	msgs, _ := s.GetUnreadMessages(id, "b")
	if len(msgs) != 0 {
		t.Errorf("expected 0 after mark read, got %d", len(msgs))
	}
}

func TestCheckPendingInvitations(t *testing.T) {
	s := mustOpen(t)

	s.StartDialog("a", "b", "pending topic")

	// B should see invitation
	d, err := s.CheckPendingInvitations("b")
	if err != nil {
		t.Fatalf("CheckPendingInvitations: %v", err)
	}
	if d == nil {
		t.Fatal("expected pending invitation for b")
	}
	if d.Topic != "pending topic" {
		t.Errorf("wrong topic: %s", d.Topic)
	}

	// A should NOT see invitation (initiator, not partner)
	d, _ = s.CheckPendingInvitations("a")
	if d != nil {
		t.Error("initiator should not get own invitation")
	}

	// C should not see it
	d, _ = s.CheckPendingInvitations("c")
	if d != nil {
		t.Error("unrelated session should not see invitation")
	}
}

func TestEndDialog(t *testing.T) {
	s := mustOpen(t)

	id, _ := s.StartDialog("a", "b", "test")
	s.UpdateDialogStatus(id, "active")
	s.UpdateDialogStatus(id, "ended")

	d, _ := s.GetDialog(id)
	if d.Status != "ended" {
		t.Errorf("expected ended, got %s", d.Status)
	}

	// Should not appear as active
	active, _ := s.GetActiveDialogForSession("a")
	if active != nil {
		t.Error("ended dialog should not appear as active")
	}

	// Should not appear as invitation
	inv, _ := s.CheckPendingInvitations("b")
	if inv != nil {
		t.Error("ended dialog should not appear as invitation")
	}
}

func TestGetChannelMessages_LegacyTruncatedTargetNotMatched(t *testing.T) {
	s := mustOpen(t)

	// A legacy row whose target was truncated by the sender to 8 characters.
	// Decision (binding): no migration — such rows become undeliverable.
	shortTarget := "a542b034"
	if _, err := s.SendChannelMessage(shortTarget, "sender-1", "hello from sender", "command"); err != nil {
		t.Fatalf("SendChannelMessage: %v", err)
	}

	fullUUID := "a542b034-d69d-4e10-9553-a549eefa4d77"
	msgs, err := s.GetChannelMessages([]string{fullUUID})
	if err != nil {
		t.Fatalf("GetChannelMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("truncated target must not be matched by the full id, got %d message(s)", len(msgs))
	}

	// The exact target still matches exactly.
	if len(s.ChannelAddresses(shortTarget)) != 1 {
		t.Fatalf("ChannelAddresses(%q) = %v, want the literal address alone", shortTarget, s.ChannelAddresses(shortTarget))
	}
	msgs, err = s.GetChannelMessages(s.ChannelAddresses(shortTarget))
	if err != nil {
		t.Fatalf("GetChannelMessages(exact): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message for the exact target, got %d", len(msgs))
	}
}

func TestGetChannelMessages_OpencodeSessionsDoNotCollide(t *testing.T) {
	s := mustOpen(t)

	targetA := "opencode:ses_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	targetB := "opencode:ses_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	s.SendChannelMessage(targetA, "sender-1", "for A", "command")
	s.SendChannelMessage(targetB, "sender-1", "for B", "command")

	msgsA, err := s.GetChannelMessages(s.ChannelAddresses(targetA))
	if err != nil {
		t.Fatalf("GetChannelMessages(A): %v", err)
	}
	if len(msgsA) != 1 || msgsA[0].Content != "for A" {
		t.Fatalf("session A received %d message(s) (%v), want exactly its own", len(msgsA), contents(msgsA))
	}

	msgsB, err := s.GetChannelMessages(s.ChannelAddresses(targetB))
	if err != nil {
		t.Fatalf("GetChannelMessages(B): %v", err)
	}
	if len(msgsB) != 1 || msgsB[0].Content != "for B" {
		t.Fatalf("session B received %d message(s) (%v), want exactly its own", len(msgsB), contents(msgsB))
	}
}

func TestGetChannelMessages_UnderscoreIsNotWildcard(t *testing.T) {
	s := mustOpen(t)

	s.SendChannelMessage("opencode:ses_ABC", "sender-1", "for the underscored session", "command")

	// Only the position of the underscore distinguishes the two addresses.
	msgs, err := s.GetChannelMessages(s.ChannelAddresses("opencode:sesXABC"))
	if err != nil {
		t.Fatalf("GetChannelMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("'_' must not act as a wildcard, got %d message(s)", len(msgs))
	}

	msgs, err = s.GetChannelMessages(s.ChannelAddresses("opencode:ses_ABC"))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("exact target should still match: %d message(s), err=%v", len(msgs), err)
	}
}

func TestGetChannelMessages_MatchesAllAgentAddresses(t *testing.T) {
	s := mustOpen(t)
	const oc = "ses_cccccccccccccccccccccccccc"
	const synthetic = "11112222-3333-4444-5555-666677778888"
	if err := s.AgentCreate(Agent{ID: "agent-x", Project: "p", Section: "s", SessionID: synthetic, OpencodeSessionID: oc, Status: "running"}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	// Addressed by the synthetic daemon UUID …
	s.SendChannelMessage(synthetic, "sender-1", "via synthetic", "command")
	// … and by the backend address.
	s.SendChannelMessage("opencode:"+oc, "sender-1", "via backend", "command")

	for _, readAs := range []string{synthetic, "opencode:" + oc, oc} {
		msgs, err := s.GetChannelMessages(s.ChannelAddresses(readAs))
		if err != nil {
			t.Fatalf("GetChannelMessages(%q): %v", readAs, err)
		}
		if len(msgs) != 2 {
			t.Errorf("reading as %q saw %d message(s) %v, want both", readAs, len(msgs), contents(msgs))
		}
	}
}

func TestSendChannelMessage_CanonicalizesBareSesTarget(t *testing.T) {
	s := mustOpen(t)

	s.SendChannelMessage("ses_ABC", "sender-1", "bare target", "command")

	msgs, err := s.GetChannelMessages([]string{"opencode:ses_ABC"})
	if err != nil {
		t.Fatalf("GetChannelMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("a bare ses_ target must land at opencode:ses_*, got %d message(s)", len(msgs))
	}

	var stored string
	if err := s.DB().QueryRow(`SELECT target FROM agent_messages ORDER BY id DESC LIMIT 1`).Scan(&stored); err != nil {
		t.Fatalf("read back target: %v", err)
	}
	if stored != "opencode:ses_ABC" {
		t.Errorf("stored target = %q, want %q", stored, "opencode:ses_ABC")
	}
}

func TestLegacyBareSesTarget_NotMatched(t *testing.T) {
	s := mustOpen(t)

	// A row written before the fix, addressed with the bare backend id.
	if _, err := s.DB().Exec(`INSERT INTO agent_messages (dialog_id, target, sender, content, msg_type) VALUES (0, ?, ?, ?, ?)`,
		"ses_LEGACY", "sender-1", "old row", "command"); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	// Neither the canonical nor the bare read form matches it: fuzzy matching is gone.
	// Decision (binding): no migration, such rows are accepted as undeliverable.
	for _, readAs := range []string{"opencode:ses_LEGACY", "ses_LEGACY"} {
		msgs, err := s.GetChannelMessages(s.ChannelAddresses(readAs))
		if err != nil {
			t.Fatalf("GetChannelMessages(%q): %v", readAs, err)
		}
		if len(msgs) != 0 {
			t.Errorf("reading as %q must not pick up the legacy bare target, got %d", readAs, len(msgs))
		}
	}
}

func contents(msgs []models.DialogMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

func TestGetChannelMessages_EmptyTargetsIsEmpty(t *testing.T) {
	s := mustOpen(t)
	s.SendChannelMessage("opencode:ses_x", "sender", "msg", "command")

	msgs, err := s.GetChannelMessages(nil)
	if err != nil || len(msgs) != 0 {
		t.Errorf("GetChannelMessages(nil) = %d message(s), err=%v", len(msgs), err)
	}
	msgs, err = s.GetUnnotifiedMessages(nil)
	if err != nil || len(msgs) != 0 {
		t.Errorf("GetUnnotifiedMessages(nil) = %d message(s), err=%v", len(msgs), err)
	}
	if err := s.MarkChannelMessagesInjected(nil); err != nil {
		t.Errorf("MarkChannelMessagesInjected(nil): %v", err)
	}
}

func TestIsAckOnAck_MixedAddressForms(t *testing.T) {
	s := mustOpen(t)
	const ocX = "ses_xxxxxxxxxxxxxxxxxxxxxxxxxx"
	const ocC = "ses_cccccccccccccccccccccccccc"
	const sX = "11111111-1111-4111-8111-111111111111"
	const sC = "22222222-2222-4222-8222-222222222222"
	for _, a := range []Agent{
		{ID: "agent-x", Project: "p", Section: "sx", SessionID: sX, OpencodeSessionID: ocX, Status: "running"},
		{ID: "agent-c", Project: "p", Section: "sc", SessionID: sC, OpencodeSessionID: ocC, Status: "running"},
	} {
		if err := s.AgentCreate(a); err != nil {
			t.Fatalf("AgentCreate: %v", err)
		}
	}

	// X acknowledged C: the row is stored under both canonical backend addresses.
	s.SendChannelMessage("opencode:"+ocC, "opencode:"+ocX, "ok", "ack")

	// C replies by addressing X with its synthetic session id — the guard must
	// still see the stored row, otherwise the ack ping-pong restarts.
	if !s.IsAckOnAck(sX, sC, "ack") {
		t.Error("ACK-on-ACK detection must span address forms")
	}
	if s.IsAckOnAck(sX, sC, "command") {
		t.Error("command after ack should not be detected as loop")
	}
}

func TestMarkChannelMessagesInjected_ExactMatch(t *testing.T) {
	s := mustOpen(t)

	// Send with full UUID target
	fullUUID := "b1234567-aaaa-bbbb-cccc-dddddddddddd"
	_, err := s.SendChannelMessage(fullUUID, "sender-1", "direct message", "command")
	if err != nil {
		t.Fatalf("SendChannelMessage: %v", err)
	}

	// Mark with exact same UUID — should work via exact match branch
	err = s.MarkChannelMessagesInjected([]string{fullUUID})
	if err != nil {
		t.Fatalf("MarkChannelMessagesInjected: %v", err)
	}

	msgs, _ := s.GetChannelMessages([]string{fullUUID})
	if len(msgs) != 0 {
		t.Errorf("expected 0 after mark read, got %d", len(msgs))
	}
}

func TestMarkChannelMessagesInjected_ScopedToTarget(t *testing.T) {
	s := mustOpen(t)

	targetA := "opencode:ses_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	targetB := "opencode:ses_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	s.SendChannelMessage(targetA, "sender-1", "for A", "command")
	s.SendChannelMessage(targetB, "sender-1", "for B", "command")

	if err := s.MarkChannelMessagesInjected(s.ChannelAddresses(targetA)); err != nil {
		t.Fatalf("MarkChannelMessagesInjected(A): %v", err)
	}

	msgsB, err := s.GetChannelMessages(s.ChannelAddresses(targetB))
	if err != nil {
		t.Fatalf("GetChannelMessages(B): %v", err)
	}
	if len(msgsB) != 1 {
		t.Fatalf("acknowledging A must not touch B: B sees %d message(s) %v", len(msgsB), contents(msgsB))
	}
}

func TestGetUnnotifiedMessages_OpencodeSessionsDoNotCollide(t *testing.T) {
	s := mustOpen(t)

	targetA := "opencode:ses_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	targetB := "opencode:ses_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	s.SendChannelMessage(targetA, "sender-1", "for A", "command")
	s.SendChannelMessage(targetB, "sender-1", "for B", "command")

	msgs, err := s.GetUnnotifiedMessages(s.ChannelAddresses(targetA))
	if err != nil {
		t.Fatalf("GetUnnotifiedMessages(A): %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "for A" {
		t.Fatalf("notified set for A = %v, want only its own message", contents(msgs))
	}
}

func TestSendChannelMessage_WithMsgType(t *testing.T) {
	s := mustOpen(t)
	msgID, err := s.SendChannelMessage("target-1", "sender-1", "hello", "command")
	if err != nil {
		t.Fatal(err)
	}
	if msgID == 0 {
		t.Fatal("expected non-zero message ID")
	}
	msgs, err := s.GetChannelMessages([]string{"target-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].MsgType != "command" {
		t.Errorf("msg_type=%q want command", msgs[0].MsgType)
	}
}

func TestSendChannelMessage_DefaultMsgType(t *testing.T) {
	s := mustOpen(t)
	msgID, err := s.SendChannelMessage("target-2", "sender-2", "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if msgID == 0 {
		t.Fatal("expected non-zero message ID")
	}
	msgs, err := s.GetChannelMessages([]string{"target-2"})
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].MsgType != "command" {
		t.Errorf("msg_type=%q want command (default)", msgs[0].MsgType)
	}
}

func TestIsAckOnAck_DetectsLoop(t *testing.T) {
	s := mustOpen(t)
	s.SendChannelMessage("session-B", "session-A", "ok", "ack")
	s.MarkChannelMessagesInjected([]string{"session-B"})
	isLoop := s.IsAckOnAck("session-A", "session-B", "ack")
	if !isLoop {
		t.Error("expected ACK-on-ACK detection, got false")
	}
}

func TestIsAckOnAck_CanonicalizesAddresses(t *testing.T) {
	s := mustOpen(t)

	// Stored with canonical addresses; the caller may address by the short form.
	s.SendChannelMessage("opencode:ses_B", "opencode:ses_A", "ok", "ack")
	s.MarkChannelMessagesInjected([]string{"opencode:ses_B"})

	if !s.IsAckOnAck("ses_A", "ses_B", "ack") {
		t.Error("ACK-on-ACK detection must be address-form independent")
	}
	if s.IsAckOnAck("ses_A", "ses_B", "command") {
		t.Error("command after ack should not be detected as loop")
	}
}

func TestIsAckOnAck_CommandAfterAck_NotLoop(t *testing.T) {
	s := mustOpen(t)
	s.SendChannelMessage("session-B", "session-A", "ok", "ack")
	s.MarkChannelMessagesInjected([]string{"session-B"})
	isLoop := s.IsAckOnAck("session-A", "session-B", "command")
	if isLoop {
		t.Error("command after ack should not be detected as loop")
	}
}

func TestIsAckOnAck_NoHistory_NotLoop(t *testing.T) {
	s := mustOpen(t)
	isLoop := s.IsAckOnAck("session-A", "session-B", "ack")
	if isLoop {
		t.Error("no history should not be detected as loop")
	}
}

func TestBroadcast(t *testing.T) {
	s := mustOpen(t)

	// Session A broadcasts
	id, err := s.SendBroadcast("session-a", "yesmem", "Schema v0.29 ist drin")
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero message ID")
	}

	// Session B sees it
	msgs, err := s.GetUnreadBroadcasts("session-b", "yesmem")
	if err != nil {
		t.Fatalf("GetUnreadBroadcasts: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(msgs))
	}
	if msgs[0].Content != "Schema v0.29 ist drin" {
		t.Errorf("wrong content: %s", msgs[0].Content)
	}

	// Session A should NOT see own broadcast
	own, _ := s.GetUnreadBroadcasts("session-a", "yesmem")
	if len(own) != 0 {
		t.Errorf("sender should not see own broadcast, got %d", len(own))
	}

	// Wrong project should not see it
	other, _ := s.GetUnreadBroadcasts("session-b", "other-project")
	if len(other) != 0 {
		t.Errorf("wrong project should not see broadcast, got %d", len(other))
	}

	// Mark read
	s.MarkBroadcastsRead([]int64{msgs[0].ID}, "session-b")
	after, _ := s.GetUnreadBroadcasts("session-b", "yesmem")
	if len(after) != 0 {
		t.Errorf("expected 0 after mark read, got %d", len(after))
	}
}

// A broadcast sent from a worktree of a project must reach the main repo, and
// the other way round: both are the same project. The comparison used to
// truncate the reader side only (its full path, its basename) while the writer
// stored whatever path it held, so a worktree broadcast stayed invisible in the
// main repo and vice versa.
func TestGetUnreadBroadcasts_WorktreeAndMainRepoShareTheProject(t *testing.T) {
	s := mustOpen(t)

	if _, err := s.SendBroadcast("agent-in-worktree", "/repo/.worktrees/yesloop-x", "from the worktree"); err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	if _, err := s.SendBroadcast("agent-in-main", "/repo", "from the main repo"); err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}

	mainMsgs, err := s.GetUnreadBroadcasts("reader-main", "/repo")
	if err != nil {
		t.Fatalf("GetUnreadBroadcasts(main): %v", err)
	}
	if len(mainMsgs) != 2 {
		t.Errorf("main repo sees %d broadcasts, want 2 (its own and the worktree's)", len(mainMsgs))
	}

	wtMsgs, err := s.GetUnreadBroadcasts("reader-worktree", "/repo/.worktrees/yesloop-x")
	if err != nil {
		t.Fatalf("GetUnreadBroadcasts(worktree): %v", err)
	}
	if len(wtMsgs) != 2 {
		t.Errorf("worktree sees %d broadcasts, want 2 (its own and the main repo's)", len(wtMsgs))
	}

	// A different project stays invisible in both directions.
	other, _ := s.GetUnreadBroadcasts("reader-main", "/other-repo")
	if len(other) != 0 {
		t.Errorf("a foreign project sees %d broadcasts, want 0", len(other))
	}
}

// A row written before writes were canonicalised sits under a worktree path. It
// is still inside its 24h read window at deploy time, so the reader has to match
// it too — the row cannot be rewritten, it is written once.
func TestGetUnreadBroadcasts_MatchesALegacyWorktreeRow(t *testing.T) {
	s := mustOpen(t)

	if _, err := s.db.Exec(`INSERT INTO agent_broadcasts (sender, project, content) VALUES (?, ?, ?)`,
		"agent-in-worktree", "/repo/.worktrees/yesloop-legacy", "written before the fix"); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.GetUnreadBroadcasts("reader-main", "/repo")
	if err != nil {
		t.Fatalf("GetUnreadBroadcasts: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("main repo sees %d legacy worktree broadcasts, want 1", len(msgs))
	}
}

// "_" is a LIKE wildcard. The read_by check matched a session id with LIKE, so
// "ses_a" also matched the stored reader "sesXa" — a session id the reader never
// used, which hid the broadcast from the session it was meant for.
func TestGetUnreadBroadcasts_UnderscoreInSessionIDIsNoWildcard(t *testing.T) {
	s := mustOpen(t)

	id, err := s.SendBroadcast("someone", "/proj", "for ses_a")
	if err != nil {
		t.Fatalf("SendBroadcast: %v", err)
	}
	// A different session marks its own mail read. Its id differs from "ses_a"
	// in exactly the character the "_" wildcard accepts.
	if err := s.MarkBroadcastsRead([]int64{id}, "sesXa"); err != nil {
		t.Fatalf("MarkBroadcastsRead: %v", err)
	}

	msgs, err := s.GetUnreadBroadcasts("ses_a", "/proj")
	if err != nil {
		t.Fatalf("GetUnreadBroadcasts: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("ses_a sees %d broadcasts, want 1 — a foreign read_by entry hid it", len(msgs))
	}
}

// The channel tables grew without bound (live 2026-10-16: 68.723 rows in
// agent_messages, 65.651 of them from the scheduler). Delivered rows older than
// the retention belong to no reader: the readers select on delivered = 0, and a
// broadcast is only readable for 24h. Rows that can still be read — undelivered
// ones — and the newest row of an address pair (IsAckOnAck reads it) stay.
func TestPruneChannelHistory_DeletesOnlyWhatNoReaderCanReach(t *testing.T) {
	s := mustOpen(t)

	deliveredOld, err := s.SendChannelMessage("opencode:ses_target", "opencode:ses_sender", "delivered and old", "command")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMessageDelivered(deliveredOld); err != nil {
		t.Fatal(err)
	}
	undeliveredOld, err := s.SendChannelMessage("opencode:ses_target2", "opencode:ses_sender", "still pending", "command")
	if err != nil {
		t.Fatal(err)
	}
	deliveredNew, err := s.SendChannelMessage("opencode:ses_target3", "opencode:ses_sender", "delivered and fresh", "command")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMessageDelivered(deliveredNew); err != nil {
		t.Fatal(err)
	}
	// The newest row of a pair survives even when it is old: IsAckOnAck reads it
	// to decide whether a new ack would close a loop.
	newestOfPair, err := s.SendChannelMessage("opencode:ses_target", "opencode:ses_sender", "newest of the pair", "ack")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMessageDelivered(newestOfPair); err != nil {
		t.Fatal(err)
	}
	oldBroadcast, err := s.SendBroadcast("someone", "/proj", "old broadcast")
	if err != nil {
		t.Fatal(err)
	}
	freshBroadcast, err := s.SendBroadcast("someone", "/proj", "fresh broadcast")
	if err != nil {
		t.Fatal(err)
	}

	backdate := func(table, column string, id int64, hours int) {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE `+table+` SET `+column+` = datetime('now', 'localtime', ?) WHERE id = ?`,
			fmt.Sprintf("-%d hours", hours), id); err != nil {
			t.Fatalf("backdate %s %d: %v", table, id, err)
		}
	}
	for _, id := range []int64{deliveredOld, undeliveredOld, newestOfPair} {
		backdate("agent_messages", "created_at", id, 48)
	}
	backdate("agent_broadcasts", "created_at", oldBroadcast, 48)

	if _, _, err := s.PruneChannelHistory(); err != nil {
		t.Fatalf("PruneChannelHistory: %v", err)
	}

	count := func(table string, id int64) int {
		t.Helper()
		var n int
		if err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count %s %d: %v", table, id, err)
		}
		return n
	}
	if count("agent_messages", deliveredOld) != 0 {
		t.Error("a delivered message older than the retention survived the prune")
	}
	if count("agent_messages", undeliveredOld) != 1 {
		t.Error("an undelivered message was pruned — its reader would never see it")
	}
	if count("agent_messages", deliveredNew) != 1 {
		t.Error("a fresh delivered message was pruned")
	}
	if count("agent_messages", newestOfPair) != 1 {
		t.Error("the newest row of an address pair was pruned — IsAckOnAck would read an older row instead")
	}
	if count("agent_broadcasts", oldBroadcast) != 0 {
		t.Error("a broadcast older than its 24h read window survived the prune")
	}
	if count("agent_broadcasts", freshBroadcast) != 1 {
		t.Error("a fresh broadcast was pruned")
	}
}

// agent_broadcasts.created_at is the UTC default (CURRENT_TIMESTAMP), while the
// message tables are written in local time. The retention has to compare in the
// column's own clock: in a zone east of UTC a local-time threshold makes a
// broadcast look older than it is, and one still inside its read window is
// pruned. Same clock mismatch as the 30-minute lock (audit F6).
func TestPruneChannelHistory_ComparesBroadcastAgeInTheColumnsClock(t *testing.T) {
	s := mustOpen(t)

	visible, err := s.SendBroadcast("someone", "/proj", "still inside the window")
	if err != nil {
		t.Fatal(err)
	}
	expired, err := s.SendBroadcast("someone", "/proj", "past the window")
	if err != nil {
		t.Fatal(err)
	}
	// UTC, the clock the column default writes.
	if _, err := s.db.Exec(`UPDATE agent_broadcasts SET created_at = datetime('now', '-23 hours') WHERE id = ?`, visible); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent_broadcasts SET created_at = datetime('now', '-25 hours') WHERE id = ?`, expired); err != nil {
		t.Fatal(err)
	}
	// The reader's own window, from the same query the proxy uses.
	msgs, err := s.GetUnreadBroadcasts("reader", "/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the 23h broadcast is not readable to begin with: %d rows", len(msgs))
	}

	if _, _, err := s.PruneChannelHistory(); err != nil {
		t.Fatalf("PruneChannelHistory: %v", err)
	}

	rows := func(id int64) int {
		t.Helper()
		var n int
		if err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM agent_broadcasts WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if rows(visible) != 1 {
		t.Error("a broadcast inside its read window was pruned — the retention compares in the wrong clock")
	}
	if rows(expired) != 0 {
		t.Error("a broadcast past its read window survived")
	}
}

// A pinged message must still be markable as delivered. The heartbeat ping sets
// read = 1 without delivered (GetUnnotifiedMessages selects on read = 0, so the
// ping does not repeat), and MarkChannelMessagesInjected selected on read = 0 — so
// the pinged row was never marked delivered and the target's proxy re-injected
// it on every request (live 2026-10-16: 162 rows with read=1, delivered=0).
func TestMarkChannelMessagesInjected_MarksAPingedMessageDelivered(t *testing.T) {
	s := mustOpen(t)

	id, err := s.SendChannelMessage("opencode:ses_target", "sender-1", "hi", "command")
	if err != nil {
		t.Fatalf("SendChannelMessage: %v", err)
	}
	// Heartbeat pings the target for this message: read = 1, delivered stays 0.
	if err := s.MarkMessageNotified(id); err != nil {
		t.Fatalf("MarkMessageNotified: %v", err)
	}
	unnotified, err := s.GetUnnotifiedMessages([]string{"opencode:ses_target"})
	if err != nil {
		t.Fatalf("GetUnnotifiedMessages: %v", err)
	}
	if len(unnotified) != 0 {
		t.Fatalf("premise broken: the ping would repeat (%d reader(s) still see it unread)", len(unnotified))
	}

	// The target's proxy pulled the content (check_channel) and acknowledges the
	// injection (mark_channel_read).
	if err := s.MarkChannelMessagesInjected([]string{"opencode:ses_target"}); err != nil {
		t.Fatalf("MarkChannelMessagesInjected: %v", err)
	}

	msgs, err := s.GetChannelMessages([]string{"opencode:ses_target"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("a pinged message stays undelivered and is injected again on every request: %d message(s) pending", len(msgs))
	}
	delivered, _, _, err := s.GetMessageDeliveryStatus(id)
	if err != nil {
		t.Fatalf("GetMessageDeliveryStatus: %v", err)
	}
	if !delivered {
		t.Error("delivered = false, want true after the reader acknowledged the message")
	}
}

// --- Claiming a message for one delivery attempt ---

// A claim must win once and lose afterwards: the two remaining consumers (proxy
// injection, heartbeat ping) select on delivered = 0, so a second winner would
// deliver the same message twice.
func TestClaimChannelMessage_WinsOnlyOnce(t *testing.T) {
	s := mustOpen(t)

	id, err := s.SendChannelMessage("opencode:ses_target", "sender-1", "hi", "command")
	if err != nil {
		t.Fatalf("SendChannelMessage: %v", err)
	}

	first, err := s.ClaimChannelMessage(id)
	if err != nil {
		t.Fatalf("ClaimChannelMessage: %v", err)
	}
	if !first {
		t.Fatal("the first claim must win")
	}
	second, err := s.ClaimChannelMessage(id)
	if err != nil {
		t.Fatalf("ClaimChannelMessage (second): %v", err)
	}
	if second {
		t.Error("a claimed message must not be claimable twice")
	}
}

// A claimed message leaves both stored-path readers, which is the whole point:
// otherwise the proxy would inject it while the push is in flight.
func TestClaimChannelMessage_HidesTheMessageFromBothReaders(t *testing.T) {
	s := mustOpen(t)

	id, _ := s.SendChannelMessage("opencode:ses_target", "sender-1", "hi", "command")
	if _, err := s.ClaimChannelMessage(id); err != nil {
		t.Fatalf("ClaimChannelMessage: %v", err)
	}

	msgs, err := s.GetChannelMessages([]string{"opencode:ses_target"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("proxy reader must not see a claimed message, got %d", len(msgs))
	}
	unnotified, err := s.GetUnnotifiedMessages([]string{"opencode:ses_target"})
	if err != nil {
		t.Fatal(err)
	}
	if len(unnotified) != 0 {
		t.Errorf("heartbeat reader must not see a claimed message, got %d", len(unnotified))
	}
}

// Releasing returns the message to the stored path, so a failed push cannot
// swallow it.
func TestReleaseChannelMessage_RestoresTheStoredPath(t *testing.T) {
	s := mustOpen(t)

	id, _ := s.SendChannelMessage("opencode:ses_target", "sender-1", "hi", "command")
	if claimed, _ := s.ClaimChannelMessage(id); !claimed {
		t.Fatal("claim must win")
	}
	if err := s.ReleaseChannelMessage(id); err != nil {
		t.Fatalf("ReleaseChannelMessage: %v", err)
	}

	msgs, err := s.GetChannelMessages([]string{"opencode:ses_target"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("a released message must be readable again, got %d", len(msgs))
	}
	delivered, _, failed, err := s.GetMessageDeliveryStatus(id)
	if err != nil {
		t.Fatal(err)
	}
	if delivered || failed {
		t.Errorf("released message must look pending, got delivered=%v failed=%v", delivered, failed)
	}
}

// A message the heartbeat already gave up on is not a delivery candidate.
func TestClaimChannelMessage_RefusesFailedMessage(t *testing.T) {
	s := mustOpen(t)

	id, _ := s.SendChannelMessage("opencode:ses_target", "sender-1", "hi", "command")
	if err := s.MarkDeliveryFailed(id); err != nil {
		t.Fatalf("MarkDeliveryFailed: %v", err)
	}

	claimed, err := s.ClaimChannelMessage(id)
	if err != nil {
		t.Fatalf("ClaimChannelMessage: %v", err)
	}
	if claimed {
		t.Error("a message marked as failed must not be claimed for delivery")
	}
}

// A lost claim must never be reported as a delivery by the other side: the
// claim is conditional, so claiming an already-delivered row reports false
// rather than silently winning.
func TestClaimChannelMessage_LosesToAnAlreadyDeliveredRow(t *testing.T) {
	s := mustOpen(t)

	id, _ := s.SendChannelMessage("opencode:ses_target", "sender-1", "hi", "command")
	if err := s.MarkChannelMessagesInjected([]string{"opencode:ses_target"}); err != nil {
		t.Fatalf("MarkChannelMessagesInjected: %v", err)
	}

	claimed, err := s.ClaimChannelMessage(id)
	if err != nil {
		t.Fatalf("ClaimChannelMessage: %v", err)
	}
	if claimed {
		t.Error("a row the proxy already delivered must not be claimable")
	}
}

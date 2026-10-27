package storage

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

const tableAgentDialogs = `CREATE TABLE IF NOT EXISTS agent_dialogs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	initiator   TEXT NOT NULL,
	partner     TEXT NOT NULL,
	topic       TEXT DEFAULT '',
	status      TEXT DEFAULT 'pending',
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
)`

const tableAgentMessages = `CREATE TABLE IF NOT EXISTS agent_messages (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	dialog_id   INTEGER NOT NULL REFERENCES agent_dialogs(id),
	target      TEXT DEFAULT '',
	sender      TEXT NOT NULL,
	content     TEXT NOT NULL,
	msg_type    TEXT DEFAULT 'command',
	read        INTEGER DEFAULT 0,
	delivered   INTEGER DEFAULT 0,
	delivered_at TEXT,
	delivery_retries INTEGER DEFAULT 0,
	delivery_failed  INTEGER DEFAULT 0,
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
)`

// StartDialog creates a new agent dialog. Returns dialog ID.
func (s *Store) StartDialog(initiator, partner, topic string) (int64, error) {
	result, err := s.db.Exec(`INSERT INTO agent_dialogs (initiator, partner, topic, status) VALUES (?, ?, ?, 'pending')`,
		initiator, partner, topic)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetDialog returns a dialog by ID.
func (s *Store) GetDialog(id int64) (*models.Dialog, error) {
	row := s.readerDB().QueryRow(`SELECT id, initiator, partner, topic, status, created_at FROM agent_dialogs WHERE id = ?`, id)
	d := &models.Dialog{}
	var ts string
	err := row.Scan(&d.ID, &d.Initiator, &d.Partner, &d.Topic, &d.Status, &ts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
	return d, nil
}

// GetActiveDialogForSession returns the active or pending dialog for a session (as initiator or partner).
func (s *Store) GetActiveDialogForSession(sessionID string) (*models.Dialog, error) {
	row := s.readerDB().QueryRow(`SELECT id, initiator, partner, topic, status, created_at FROM agent_dialogs WHERE (initiator = ? OR partner = ?) AND status IN ('pending', 'active') ORDER BY created_at DESC LIMIT 1`,
		sessionID, sessionID)
	d := &models.Dialog{}
	var ts string
	err := row.Scan(&d.ID, &d.Initiator, &d.Partner, &d.Topic, &d.Status, &ts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
	return d, nil
}

// UpdateDialogStatus sets the status of a dialog (pending → active → ended).
func (s *Store) UpdateDialogStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE agent_dialogs SET status = ? WHERE id = ?`, status, id)
	return err
}

// SendDialogMessage writes a message to a dialog.
func (s *Store) SendDialogMessage(dialogID int64, sender, content string) (int64, error) {
	result, err := s.db.Exec(`INSERT INTO agent_messages (dialog_id, sender, content, created_at) VALUES (?, ?, ?, datetime('now', 'localtime'))`,
		dialogID, sender, content)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetUnreadMessages returns unread messages for a session in a dialog (messages NOT sent by this session).
func (s *Store) GetUnreadMessages(dialogID int64, forSession string) ([]models.DialogMessage, error) {
	rows, err := s.readerDB().Query(`SELECT id, dialog_id, sender, content, read, created_at FROM agent_messages WHERE dialog_id = ? AND sender != ? AND read = 0 ORDER BY created_at ASC`,
		dialogID, forSession)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []models.DialogMessage
	for rows.Next() {
		m := models.DialogMessage{}
		var ts string
		var readInt int
		if err := rows.Scan(&m.ID, &m.DialogID, &m.Sender, &m.Content, &readInt, &ts); err != nil {
			continue
		}
		m.Read = readInt != 0
		m.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// MarkMessagesRead marks all messages in a dialog as read for a session.
func (s *Store) MarkMessagesRead(dialogID int64, forSession string) error {
	_, err := s.db.Exec(`UPDATE agent_messages SET read = 1 WHERE dialog_id = ? AND sender != ? AND read = 0`,
		dialogID, forSession)
	return err
}

// CheckPendingInvitations returns a pending dialog where this session is the partner.
func (s *Store) CheckPendingInvitations(sessionID string) (*models.Dialog, error) {
	row := s.readerDB().QueryRow(`SELECT id, initiator, partner, topic, status, created_at FROM agent_dialogs WHERE partner = ? AND status = 'pending' ORDER BY created_at DESC LIMIT 1`,
		sessionID)
	d := &models.Dialog{}
	var ts string
	err := row.Scan(&d.ID, &d.Initiator, &d.Partner, &d.Topic, &d.Status, &ts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
	return d, nil
}

const tableAgentBroadcasts = `CREATE TABLE IF NOT EXISTS agent_broadcasts (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	sender      TEXT NOT NULL,
	project     TEXT NOT NULL,
	content     TEXT NOT NULL,
	read_by     TEXT DEFAULT '',
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
)`

// broadcastProjectKey reduces a project identifier to the form every session of
// that project shares. A loop agent runs in a worktree of the repository while
// naming the repository as its project, and both the full path and the bare
// basename are in use — so a broadcast written under a worktree path stayed
// invisible in the main repo even though both name one project.
//
// The full path is kept outside a worktree: models.CanonicalProject performs the
// same strip but returns the basename, which is the right grouping key for a
// whole project and the wrong one here, where two distinct repos may share it.
func broadcastProjectKey(project string) string {
	p := strings.TrimRight(strings.TrimSpace(project), "/")
	if i := strings.Index(p, "/.worktrees/"); i > 0 {
		return p[:i]
	}
	return p
}

// SendBroadcast writes a broadcast message visible to all sessions on the same project.
func (s *Store) SendBroadcast(sender, project, content string) (int64, error) {
	result, err := s.db.Exec(`INSERT INTO agent_broadcasts (sender, project, content) VALUES (?, ?, ?)`,
		sender, broadcastProjectKey(project), content)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetUnreadBroadcasts returns broadcast messages not sent by this session, created in the last 24h.
func (s *Store) GetUnreadBroadcasts(forSession, project string) ([]models.DialogMessage, error) {
	key := broadcastProjectKey(project)
	short := filepath.Base(key)
	// Four project forms are matched: the canonical key, its basename (a caller
	// may name either form), the reader's own raw string, and — for rows written
	// before writes were canonicalised — any worktree path of this project.
	// instr is literal where a LIKE pattern would treat "_" as a wildcard.
	// read_by is a comma-separated list of session ids, matched on a
	// delimiter-padded copy for the same reason: a session id contains "_", so a
	// LIKE match let one session's entry hide the broadcast from another session
	// whose id differed in that character.
	rows, err := s.readerDB().Query(`SELECT id, sender, content, created_at FROM agent_broadcasts WHERE sender != ? AND (project = ? OR project = ? OR project = ? OR instr(project, ?) = 1) AND created_at > datetime('now', '-24 hours') AND instr(',' || read_by || ',', ',' || ? || ',') = 0 ORDER BY created_at ASC`,
		forSession, key, short, strings.TrimSpace(project), key+"/.worktrees/", forSession)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []models.DialogMessage
	for rows.Next() {
		m := models.DialogMessage{}
		var ts string
		if err := rows.Scan(&m.ID, &m.Sender, &m.Content, &ts); err != nil {
			continue
		}
		m.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// MarkBroadcastsRead marks broadcast messages as read for a session.
func (s *Store) MarkBroadcastsRead(ids []int64, sessionID string) error {
	for _, id := range ids {
		s.db.Exec(`UPDATE agent_broadcasts SET read_by = CASE WHEN read_by = '' THEN ? ELSE read_by || ',' || ? END WHERE id = ?`,
			sessionID, sessionID, id)
	}
	return nil
}

// channelRetentionHours is how long a delivered channel message and a broadcast
// are kept. A broadcast is only readable for 24h (the read window in
// GetUnreadBroadcasts), and a delivered message belongs to no reader: the channel
// readers select on delivered = 0.
const channelRetentionHours = 24

// PruneChannelHistory drops the channel rows no reader can reach any more:
// broadcasts outside their 24h read window and delivered (or permanently failed)
// messages older than the retention. Undelivered messages stay — their reader
// has not seen them — and so does the newest row of every address pair, because
// IsAckOnAck reads it to decide whether a further ack would close a loop.
// Returns the number of messages and broadcasts deleted.
//
// Without it the tables grew without bound: 68.723 rows in agent_messages on
// 2026-10-16, 65.651 of them written by the scheduler, none ever deleted.
func (s *Store) PruneChannelHistory() (messages, broadcasts int64, err error) {
	// agent_broadcasts.created_at is the UTC default of the table
	// (CURRENT_TIMESTAMP) and its reader compares in UTC too; agent_messages is
	// written with datetime('now','localtime'). Each retention therefore runs in
	// the clock its column was written in.
	res, err := s.db.Exec(`DELETE FROM agent_broadcasts WHERE created_at < datetime('now', ?)`,
		fmt.Sprintf("-%d hours", channelRetentionHours))
	if err != nil {
		return 0, 0, err
	}
	broadcasts, _ = res.RowsAffected()

	res, err = s.db.Exec(`DELETE FROM agent_messages
		WHERE (delivered = 1 OR delivery_failed = 1)
		  AND created_at < datetime('now', 'localtime', ?)
		  AND EXISTS (
			SELECT 1 FROM agent_messages newer
			WHERE newer.target = agent_messages.target
			  AND newer.sender = agent_messages.sender
			  AND newer.id > agent_messages.id)`,
		fmt.Sprintf("-%d hours", channelRetentionHours))
	if err != nil {
		return 0, broadcasts, err
	}
	messages, _ = res.RowsAffected()
	return messages, broadcasts, nil
}

// --- Channel-based messaging (target-routed, no dialog state) ---

// SendChannelMessage inserts a message targeted at a specific session.
// target and sender are normalised with CanonicalAddress, so the stored address
// is the same form the reader matches against.
// msgType is one of: command, response, ack, status. Empty string defaults to "command".
func (s *Store) SendChannelMessage(target, sender, content, msgType string) (int64, error) {
	if msgType == "" {
		msgType = "command"
	}
	result, err := s.db.Exec(`INSERT INTO agent_messages (dialog_id, target, sender, content, msg_type, created_at) VALUES (0, ?, ?, ?, ?, datetime('now', 'localtime'))`,
		CanonicalAddress(target), CanonicalAddress(sender), content, msgType)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetChannelMessages returns undelivered messages addressed to any of the given
// addresses. Matching is exact: a message belongs to exactly one address, and
// "_" is a literal character, not a wildcard. Callers expand a session id into
// its address set with ChannelAddresses.
func (s *Store) GetChannelMessages(targets []string) ([]models.DialogMessage, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	rows, err := s.readerDB().Query(`SELECT id, sender, content, COALESCE(msg_type, 'command'), created_at FROM agent_messages WHERE target IN (`+sqlPlaceholders(len(targets))+`) AND delivered = 0 AND delivery_failed = 0 ORDER BY created_at ASC`,
		toArgs(targets)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []models.DialogMessage
	for rows.Next() {
		m := models.DialogMessage{}
		var ts string
		if err := rows.Scan(&m.ID, &m.Sender, &m.Content, &m.MsgType, &ts); err != nil {
			continue
		}
		m.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// MarkMessageDelivered marks a single message as successfully delivered via socket inject.
func (s *Store) MarkMessageDelivered(messageID int64) error {
	_, err := s.db.Exec(`UPDATE agent_messages SET delivered = 1, delivered_at = datetime('now', 'localtime'), read = 1 WHERE id = ?`, messageID)
	return err
}

// MarkMessageNotified sets read=1 (heartbeat pinged) but NOT delivered.
// The proxy will handle actual content delivery.
func (s *Store) MarkMessageNotified(messageID int64) error {
	_, err := s.db.Exec(`UPDATE agent_messages SET read = 1 WHERE id = ? AND delivered = 0`, messageID)
	return err
}

// ClaimChannelMessage takes a pending message for one delivery attempt and
// reports whether the claim was won.
//
// Claiming BEFORE the delivery is attempted is what keeps a second consumer —
// the proxy injection, the heartbeat ping — from carrying the same message
// while the push is in flight: both read delivered = 0, so a mark applied
// afterwards cannot tell a race from a win, and the message would be delivered
// twice. A false result means somebody else already took it.
func (s *Store) ClaimChannelMessage(messageID int64) (bool, error) {
	result, err := s.db.Exec(`UPDATE agent_messages SET delivered = 1, delivered_at = datetime('now', 'localtime'), read = 1 WHERE id = ? AND delivered = 0 AND delivery_failed = 0`, messageID)
	if err != nil {
		return false, err
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return claimed == 1, nil
}

// ReleaseChannelMessage undoes a claim whose delivery did not happen, so the
// stored path can still carry the message. Without it a failed push would look
// delivered and the message would never arrive at all.
func (s *Store) ReleaseChannelMessage(messageID int64) error {
	_, err := s.db.Exec(`UPDATE agent_messages SET delivered = 0, delivered_at = NULL, read = 0 WHERE id = ?`, messageID)
	return err
}

// GetUnnotifiedMessages returns messages addressed to any of the given
// addresses that haven't been pinged yet (read=0, delivered=0).
// Used by heartbeat to avoid repeated pings.
func (s *Store) GetUnnotifiedMessages(targets []string) ([]models.DialogMessage, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	rows, err := s.readerDB().Query(`SELECT id, sender, content, COALESCE(msg_type, 'command'), created_at FROM agent_messages WHERE target IN (`+sqlPlaceholders(len(targets))+`) AND delivered = 0 AND read = 0 AND delivery_failed = 0 ORDER BY created_at ASC`,
		toArgs(targets)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []models.DialogMessage
	for rows.Next() {
		m := models.DialogMessage{}
		var ts string
		if err := rows.Scan(&m.ID, &m.Sender, &m.Content, &m.MsgType, &ts); err != nil {
			continue
		}
		m.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// IncrementDeliveryRetry increments the retry counter for a message.
// Returns the new retry count.
func (s *Store) IncrementDeliveryRetry(messageID int64) (int, error) {
	_, err := s.db.Exec(`UPDATE agent_messages SET delivery_retries = delivery_retries + 1 WHERE id = ?`, messageID)
	if err != nil {
		return 0, err
	}
	var count int
	err = s.readerDB().QueryRow(`SELECT delivery_retries FROM agent_messages WHERE id = ?`, messageID).Scan(&count)
	return count, err
}

// MarkDeliveryFailed marks a message as permanently undeliverable.
func (s *Store) MarkDeliveryFailed(messageID int64) error {
	_, err := s.db.Exec(`UPDATE agent_messages SET delivery_failed = 1 WHERE id = ?`, messageID)
	return err
}

// GetMessageDeliveryStatus returns delivery status for a message.
func (s *Store) GetMessageDeliveryStatus(messageID int64) (delivered bool, retries int, failed bool, err error) {
	err = s.readerDB().QueryRow(`SELECT delivered, delivery_retries, delivery_failed FROM agent_messages WHERE id = ?`, messageID).Scan(&delivered, &retries, &failed)
	return
}

// MarkChannelMessagesInjected marks the messages a reader was handed as read AND
// delivered. It is the reader's side of the channel: the target pulled the content
// (check_channel) and confirms the injection (mark_channel_read) — the proxy does
// exactly that after putting the content into the system prompt.
//
// Sets both read=1 and delivered=1 to prevent re-injection by both proxy (read filter) and
// heartbeat (delivered filter). Without delivered=1, messages loop: proxy injects → session
// echoes → new message → proxy re-injects old (still delivered=0) → infinite echo loop.
// The selection is on delivered = 0, not on read = 0: the heartbeat ping sets
// read = 1 without delivered (MarkMessageNotified), so a reader keyed on read = 0
// never closed those rows and the proxy re-injected them on every request (YM-58).
// The send_to ack uses MarkChannelInboxRead instead: it must not consume a
// message whose content was never injected, either.
// Matching is exact — see GetChannelMessages.
func (s *Store) MarkChannelMessagesInjected(targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE agent_messages SET read = 1, delivered = 1, delivered_at = datetime('now', 'localtime') WHERE target IN (`+sqlPlaceholders(len(targets))+`) AND delivered = 0`,
		toArgs(targets)...)
	return err
}

// MarkChannelInboxRead marks the unread messages of an inbox as read and
// delivered. It is the implicit ack of send_to: the caller is answering, so its
// own pending mail counts as handled.
//
// The selection stays on read = 0, unlike MarkChannelMessagesInjected. A message the
// heartbeat only pinged (read = 1, delivered = 0) has promised content that has
// not been injected yet: an ack that consumed it would make the message
// unreachable for the proxy (GetChannelMessages selects on delivered = 0), so the
// sender would never see the message it was told about (YM-58 follow-up, found in
// review).
func (s *Store) MarkChannelInboxRead(targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE agent_messages SET read = 1, delivered = 1, delivered_at = datetime('now', 'localtime') WHERE target IN (`+sqlPlaceholders(len(targets))+`) AND read = 0`,
		toArgs(targets)...)
	return err
}

// IsAckOnAck returns true if sending msgType from target to sender would create an ACK loop.
// An ACK loop occurs when the last message target→sender was already an ack/status,
// and the new outgoing message is also an ack/status — both sides just echoing back.
// Both sides are compared as whole address sets, so the verdict does not depend on
// which address form a session used to send or to be addressed.
func (s *Store) IsAckOnAck(target, sender, msgType string) bool {
	if msgType != "ack" && msgType != "status" {
		return false
	}
	toMe := s.ChannelAddresses(sender)
	fromThem := s.ChannelAddresses(target)
	if len(toMe) == 0 || len(fromThem) == 0 {
		return false
	}
	var lastType string
	err := s.readerDB().QueryRow(
		`SELECT COALESCE(msg_type, 'command') FROM agent_messages WHERE target IN (`+sqlPlaceholders(len(toMe))+`) AND sender IN (`+sqlPlaceholders(len(fromThem))+`) ORDER BY id DESC LIMIT 1`,
		append(toArgs(toMe), toArgs(fromThem)...)...).Scan(&lastType)
	if err != nil {
		return false
	}
	return lastType == "ack" || lastType == "status"
}

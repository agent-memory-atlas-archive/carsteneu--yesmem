package daemon

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los13dPendingSession writes a session plus its messages: textMsgs carry text
// content, otherMsgs are tool calls without content. Only the text messages can
// become part of a narrative.
func los13dPendingSession(t *testing.T, s *storage.Store, id string, textMsgs, otherMsgs int) models.Session {
	t.Helper()
	return los13dPendingSessionAt(t, s, id, "/work/narrative", textMsgs, otherMsgs)
}

// los13dPendingSessionAt is los13dPendingSession with an explicit project path, so
// a test can place sessions in different worktrees of one repository.
func los13dPendingSessionAt(t *testing.T, s *storage.Store, id, project string, textMsgs, otherMsgs int) models.Session {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	sess := models.Session{
		ID: id, Project: project, MessageCount: textMsgs + otherMsgs,
		StartedAt: now.Add(-time.Hour), EndedAt: now,
		JSONLPath: "/" + id + ".jsonl", IndexedAt: now,
	}
	if err := s.UpsertSession(&sess); err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
	msgs := make([]models.Message, 0, textMsgs+otherMsgs)
	for i := 0; i < textMsgs; i++ {
		msgs = append(msgs, models.Message{
			SessionID: id, Role: "user", MessageType: "text",
			Content: fmt.Sprintf("text message %d", i), Timestamp: now, Sequence: i,
		})
	}
	for i := 0; i < otherMsgs; i++ {
		msgs = append(msgs, models.Message{
			SessionID: id, Role: "assistant", MessageType: "tool_use",
			ToolName: "bash", Timestamp: now, Sequence: textMsgs + i,
		})
	}
	if len(msgs) > 0 {
		if err := s.InsertMessages(msgs); err != nil {
			t.Fatalf("insert messages for %s: %v", id, err)
		}
	}
	return sess
}

// The pending set is the generator's input: a session without enough text
// messages could never be summarized, so it must not be reported as pending work
// (4389 such sessions on the live DB, 0 of them narratable).
func TestPendingNarratives_RequiresTextMessages(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	los13dPendingSession(t, s, "sess-tool-heavy", 2, 4)
	los13dPendingSession(t, s, "sess-text-rich", storage.ShortSessionMaxMessages, 0)

	pending, err := pendingNarrativeSessions(s)
	if err != nil {
		t.Fatalf("pending narratives: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("pending narratives = %v, want exactly the text-rich session: a session below the text-message threshold can never be summarized", pending)
	}
	if _, ok := pending["sess-text-rich"]; !ok {
		t.Errorf("pending = %v, want sess-text-rich", pending)
	}
}

// Crash recovery quarantines the learnings of a failed session. A narrative
// written afterwards would carry that content straight back into recall.
func TestPendingNarratives_SkipsQuarantinedSessions(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	sess := los13dPendingSession(t, s, "sess-quarantined", storage.ShortSessionMaxMessages, 0)
	if _, err := s.InsertLearning(&models.Learning{
		Category: "pattern", Content: "unreliable claim of a crashed agent",
		Project: "/work/narrative", SessionID: sess.ID,
		CreatedAt: time.Now().Truncate(time.Second), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	if _, err := s.QuarantineSession(sess.ID); err != nil {
		t.Fatalf("quarantine session: %v", err)
	}

	count, err := pendingNarrativeSessions(s)
	if err != nil {
		t.Fatalf("pending narratives: %v", err)
	}
	if len(count) != 0 {
		t.Errorf("pending = %v, want none for a quarantined session", count)
	}
}

// The quarantine mark exists to keep a failed session's content out of recall. The
// regenerate command is a second entry point into narrative generation, so it has to
// honour the same gate as the batch path; otherwise an explicit regeneration puts the
// quarantined content back.
func TestRegenerateNarrative_SkipsQuarantinedSession(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	sess := los13dPendingSession(t, s, "sess-regen-quarantined", storage.ShortSessionMaxMessages, 0)
	if _, err := s.InsertLearning(&models.Learning{
		Category: "pattern", Content: "unreliable claim of a crashed agent",
		Project: "/work/narrative", SessionID: sess.ID,
		CreatedAt: time.Now().Truncate(time.Second), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	if _, err := s.QuarantineSession(sess.ID); err != nil {
		t.Fatalf("quarantine session: %v", err)
	}

	if RegenerateNarrativeForSession(s, sess, los13cNarrativeClient{}) {
		t.Error("a quarantined session must not get a narrative")
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE category = 'narrative'`).Scan(&rows); err != nil {
		t.Fatalf("count narratives: %v", err)
	}
	if rows != 0 {
		t.Errorf("narrative rows = %d, want none for a quarantined session", rows)
	}
}

// End-to-end: only the session the pending set names may reach the LLM. The
// tool-heavy session stays untouched instead of being loaded again on every cycle.
func TestGenerateMissingNarratives_OnlyProcessesThePendingSet(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	rich := los13dPendingSession(t, s, "sess-rich", storage.ShortSessionMaxMessages, 0)
	heavy := los13dPendingSession(t, s, "sess-heavy", 2, 4)

	pending, err := pendingNarrativeSessions(s)
	if err != nil {
		t.Fatalf("pending narratives: %v", err)
	}
	GenerateMissingNarratives(s, []models.Session{rich, heavy}, nil, los13cNarrativeClient{}, pending)

	rows, err := s.DB().Query(`SELECT session_id FROM learnings WHERE category = 'narrative'`)
	if err != nil {
		t.Fatalf("read narratives: %v", err)
	}
	defer rows.Close()
	var narrated []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan narrative: %v", err)
		}
		narrated = append(narrated, id)
	}
	if len(narrated) != 1 || narrated[0] != rich.ID {
		t.Fatalf("narrated sessions = %v, want only %s", narrated, rich.ID)
	}

	var heavyMarker sql.NullString
	if err := s.DB().QueryRow(`SELECT narrative_at FROM sessions WHERE id = ?`, heavy.ID).Scan(&heavyMarker); err != nil {
		t.Fatalf("read narrative_at: %v", err)
	}
	if heavyMarker.Valid {
		t.Error("a session the generator skipped must not carry a narrative_at marker")
	}
}

// An empty pending set must be a no-op: a failed pending lookup leaves a nil map
// and the generator still runs on every cycle.
func TestGenerateMissingNarratives_EmptyPendingSet(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	sess := los13dPendingSession(t, s, "sess-open", storage.ShortSessionMaxMessages, 0)
	GenerateMissingNarratives(s, []models.Session{sess}, nil, los13cNarrativeClient{}, nil)

	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE category = 'narrative'`).Scan(&rows); err != nil {
		t.Fatalf("count narratives: %v", err)
	}
	if rows != 0 {
		t.Errorf("narrative rows = %d, want none for an empty pending set", rows)
	}
}

// The prune must not eat the row a run just wrote: a narrative carries the
// session's start time, so a run over several sessions of one project would
// otherwise delete the row written for the oldest one and leave its marker behind.
func TestGenerateMissingNarratives_KeepsEveryStoryItWrote(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	base := time.Now().Truncate(time.Second).Add(-time.Hour)
	sessions := make([]models.Session, 0, 3)
	for i := 0; i < 3; i++ {
		sess := los13dPendingSession(t, s, fmt.Sprintf("sess-%d", i), storage.ShortSessionMaxMessages, 0)
		sess.StartedAt = base.Add(-time.Duration(i) * time.Hour)
		sessions = append(sessions, sess)
	}

	pending, err := pendingNarrativeSessions(s)
	if err != nil {
		t.Fatalf("pending narratives: %v", err)
	}
	GenerateMissingNarratives(s, sessions, nil, los13cNarrativeClient{}, pending)

	var withoutRow int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sessions s
		WHERE s.narrative_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM learnings l WHERE l.session_id = s.id AND l.category = 'narrative')`).Scan(&withoutRow); err != nil {
		t.Fatalf("count markers without a row: %v", err)
	}
	if withoutRow != 0 {
		t.Errorf("%d sessions carry a narrative_at marker without a narrative row", withoutRow)
	}
}

// The prune scope is the repository, but the run's written rows were grouped per
// worktree path: a run over five worktrees of one repository pruned with only one
// worktree's ids spared and deleted the stories the other four workers had just
// written, while their sessions kept the marker.
func TestGenerateMissingNarratives_KeepsStoriesOfEveryWorktree(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("create git dir: %v", err)
	}
	sessions := make([]models.Session, 0, 5)
	for i := 0; i < 5; i++ {
		wt := filepath.Join(repo, ".worktrees", fmt.Sprintf("w%d", i))
		sessions = append(sessions, los13dPendingSessionAt(t, s, fmt.Sprintf("sess-wt-%d", i), wt, storage.ShortSessionMaxMessages, 0))
	}

	pending, err := pendingNarrativeSessions(s)
	if err != nil {
		t.Fatalf("pending narratives: %v", err)
	}
	GenerateMissingNarratives(s, sessions, nil, los13cNarrativeClient{}, pending)

	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE category = 'narrative'`).Scan(&rows); err != nil {
		t.Fatalf("count narratives: %v", err)
	}
	if rows != len(sessions) {
		t.Errorf("narrative rows = %d, want %d: a prune must not delete the rows its own run wrote in other worktrees", rows, len(sessions))
	}
	var withoutRow int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sessions s
		WHERE s.narrative_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM learnings l WHERE l.session_id = s.id AND l.category = 'narrative')`).Scan(&withoutRow); err != nil {
		t.Fatalf("count markers without a row: %v", err)
	}
	if withoutRow != 0 {
		t.Errorf("%d sessions carry a narrative_at marker without a narrative row", withoutRow)
	}
}

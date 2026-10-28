package daemon

import (
	"database/sql"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los13cNarrativeClient answers every narrative prompt with a fixed story.
type los13cNarrativeClient struct{}

func (los13cNarrativeClient) Complete(system, userMsg string, opts ...extraction.CallOption) (string, error) {
	return "Hey, it's me from 2026-10-18 10:00. I traced the cleanup to the marker. The marker was the trap. Now I know better. Next time I check it first.", nil
}

func (los13cNarrativeClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...extraction.CallOption) (string, error) {
	return "", nil
}

func (los13cNarrativeClient) Name() string  { return "los13c-narrative" }
func (los13cNarrativeClient) Model() string { return "los13c-narrative" }

// A regenerated narrative has to carry its marker: without it the batch generator
// reads the session as unnarrated and writes a second story for the same session.
func TestRegenerateNarrativeForSession_MarksTheSession(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	now := time.Now()
	sess := models.Session{
		ID: "sess-regen-marker", Project: "/test", ProjectShort: "test",
		MessageCount: storage.ShortSessionMaxMessages,
		StartedAt:    now.Add(-time.Hour), EndedAt: now,
		JSONLPath: "/sess-regen-marker.jsonl", IndexedAt: now,
	}
	if err := s.UpsertSession(&sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	msgs := make([]models.Message, 0, storage.ShortSessionMaxMessages)
	for i := 1; i <= storage.ShortSessionMaxMessages; i++ {
		msgs = append(msgs, models.Message{
			SessionID: sess.ID, Role: "user", MessageType: "text",
			Content: "message for the narrative", Timestamp: now, Sequence: i,
		})
	}
	if err := s.InsertMessages(msgs); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	if !RegenerateNarrativeForSession(s, sess, los13cNarrativeClient{}) {
		t.Fatal("narrative was not regenerated for a session that carries enough messages")
	}

	var marker sql.NullString
	if err := s.DB().QueryRow(`SELECT narrative_at FROM sessions WHERE id = ?`, sess.ID).Scan(&marker); err != nil {
		t.Fatalf("read narrative_at: %v", err)
	}
	if !marker.Valid {
		t.Error("a session with a fresh narrative must carry narrative_at")
	}
}

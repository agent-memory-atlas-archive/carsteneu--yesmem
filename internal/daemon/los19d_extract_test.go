package daemon

import (
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los19dPartialClient fails the first chunk of a pass and answers the rest, so
// the pass is partial rather than broken. Name "cli" keeps the phase at one
// worker and the call order stable.
type los19dPartialClient struct{ calls int32 }

func (c *los19dPartialClient) Complete(string, string, ...extraction.CallOption) (string, error) {
	return "", nil
}

func (c *los19dPartialClient) CompleteJSON(string, string, map[string]any, ...extraction.CallOption) (string, error) {
	if atomic.AddInt32(&c.calls, 1) == 1 {
		return "", errors.New("first chunk timed out")
	}
	return `{"learnings":[]}`, nil
}

func (c *los19dPartialClient) Name() string  { return "cli" }
func (c *los19dPartialClient) Model() string { return "los19d-partial" }

// los19dLongSession seeds one finished session whose messages are long enough to
// be split into several chunks at a chunk size of 100 tokens.
func los19dLongSession(t *testing.T, s *storage.Store, id string) models.Session {
	t.Helper()
	now := time.Now()
	const messageCount = 6
	sess := &models.Session{
		ID: id, Project: "/work/los19d", ProjectShort: "los19d",
		MessageCount: messageCount,
		StartedAt:    now.Add(-48 * time.Hour), EndedAt: now.Add(-47 * time.Hour),
		JSONLPath: "/" + id + ".jsonl", IndexedAt: now,
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
	msgs := make([]models.Message, 0, messageCount)
	for i := 1; i <= messageCount; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		msgs = append(msgs, models.Message{
			SessionID: id, Role: role, MessageType: "text",
			Content: strings.Repeat("wort ", 50), Timestamp: now, Sequence: i,
		})
	}
	if err := s.InsertMessages(msgs); err != nil {
		t.Fatalf("insert messages for %s: %v", id, err)
	}
	stored, err := s.GetSession(id)
	if err != nil || stored == nil {
		t.Fatalf("get session %s: %v", id, err)
	}
	return *stored
}

// YM-362 Key 2: a partial extraction stayed invisible to the attempt budget and
// to the session state, so the batch path certified the session as extracted
// although chunks failed. The attempt has to be counted and extracted_at has to
// stay empty, otherwise nothing ever retries the missing chunks.
func TestLos19d_PartialExtractionStaysPendingAndCountsAnAttempt(t *testing.T) {
	s := mustStore(t)
	sess := los19dLongSession(t, s, "los19d-partial-daemon")
	client := &los19dPartialClient{}
	ext := extraction.NewExtractor(client, s)
	ext.SetChunkSize(100)

	RunExtractionPhase(ext, s, []models.Session{sess}, &config.Config{}, client)

	var attempts int
	var extractedAt sql.NullString
	if err := s.DB().QueryRow(`SELECT extraction_attempts, extracted_at FROM sessions WHERE id = ?`, sess.ID).
		Scan(&attempts, &extractedAt); err != nil {
		t.Fatalf("read session state: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("extraction_attempts = %d after one partial pass, want 1", attempts)
	}
	if extractedAt.Valid {
		t.Fatalf("a partial pass certified the session as extracted at %q", extractedAt.String)
	}
	if got := atomic.LoadInt32(&client.calls); got < 2 {
		t.Fatalf("only %d chunk(s) attempted: the session was not chunked, the case is untested", got)
	}
}

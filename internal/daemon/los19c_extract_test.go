package daemon

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los19cSessionWithMessages seeds one finished session with enough messages to
// pass the extraction selection threshold (six), so the phase works on a session
// it would really be handed.
func los19cSessionWithMessages(t *testing.T, s *storage.Store, id string) models.Session {
	t.Helper()
	now := time.Now()
	const messages = 6
	sess := &models.Session{
		ID: id, Project: "/work/los19c", ProjectShort: "los19c",
		MessageCount: messages,
		StartedAt:    now.Add(-48 * time.Hour), EndedAt: now.Add(-47 * time.Hour),
		JSONLPath: "/" + id + ".jsonl", IndexedAt: now,
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
	msgs := make([]models.Message, 0, messages)
	for i := 1; i <= messages; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		msgs = append(msgs, models.Message{
			SessionID: id, Role: role, MessageType: "text",
			Content: "Bitte pruefe, ob die Extraktion dieser Sitzung etwas Ergibt", Timestamp: now, Sequence: i,
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

// los19cFailingExtractor reports a transport failure for every session and counts
// the calls.
type los19cFailingExtractor struct{ calls int32 }

func (e *los19cFailingExtractor) ExtractFromSession(string, []models.Message) ([]models.Learning, error) {
	return nil, errors.New("upstream 500")
}

func (e *los19cFailingExtractor) ExtractAndStore(string, string, []models.Message, bool) error {
	atomic.AddInt32(&e.calls, 1)
	return errors.New("upstream 500")
}

func (e *los19cFailingExtractor) SetChunkSize(int) {}

// los19cGarbageClient answers every call with text that is not extraction JSON.
// Name() reports "cli" so the phase runs a single worker and the order is stable.
type los19cGarbageClient struct{ calls int32 }

func (c *los19cGarbageClient) Complete(string, string, ...extraction.CallOption) (string, error) {
	return "not json", nil
}

func (c *los19cGarbageClient) CompleteJSON(string, string, map[string]any, ...extraction.CallOption) (string, error) {
	atomic.AddInt32(&c.calls, 1)
	return "not json at all", nil
}

func (c *los19cGarbageClient) Name() string  { return "cli" }
func (c *los19cGarbageClient) Model() string { return "los19c-garbage" }

// los19cDownClient answers every call with a transport error.
type los19cDownClient struct{ calls int32 }

func (c *los19cDownClient) Complete(string, string, ...extraction.CallOption) (string, error) {
	atomic.AddInt32(&c.calls, 1)
	return "", errors.New("api call: connection refused")
}

func (c *los19cDownClient) CompleteJSON(string, string, map[string]any, ...extraction.CallOption) (string, error) {
	return c.Complete("", "")
}

func (c *los19cDownClient) Name() string  { return "cli" }
func (c *los19cDownClient) Model() string { return "los19c-down" }

// YM-317 Key 1: a session whose extraction keeps failing stays pending and is
// retried every cycle. After a bounded number of attempts it must be marked
// skipped instead, and no later cycle may hand it to the extractor again.
func TestLos19c_ExtractionStopsAfterTheAttemptLimit(t *testing.T) {
	s := mustStore(t)
	sess := los19cSessionWithMessages(t, s, "los19c-attempts")
	ext := &los19cFailingExtractor{}

	const attempts = 5
	for i := 0; i < attempts; i++ {
		RunExtractionPhase(ext, s, []models.Session{sess}, &config.Config{}, nil)
	}

	// The counter is the persistent mark: it survives the cycle that wrote it.
	readAttempts := func() int {
		t.Helper()
		var stored int
		if err := s.DB().QueryRow(`SELECT extraction_attempts FROM sessions WHERE id = ?`, sess.ID).Scan(&stored); err != nil {
			t.Fatalf("read extraction_attempts: %v", err)
		}
		return stored
	}
	if got := readAttempts(); got != attempts {
		t.Fatalf("extraction_attempts = %d, want %d", got, attempts)
	}
	if got := atomic.LoadInt32(&ext.calls); got != attempts {
		t.Fatalf("extractor was called %d times, want %d", got, attempts)
	}

	// One more cycle must not spend another attempt on the exhausted session.
	RunExtractionPhase(ext, s, []models.Session{sess}, &config.Config{}, nil)
	if got := atomic.LoadInt32(&ext.calls); got != attempts {
		t.Fatalf("a later cycle called the extractor again (%d calls), want it to stay at %d", got, attempts)
	}

	// An explicit re-extraction is the way back and must hand out a fresh budget.
	if err := s.ResetSessionExtracted(sess.ID); err != nil {
		t.Fatalf("reset session: %v", err)
	}
	if got := readAttempts(); got != 0 {
		t.Fatalf("ResetSessionExtracted left extraction_attempts at %d, want 0", got)
	}
	RunExtractionPhase(ext, s, []models.Session{sess}, &config.Config{}, nil)
	if got := atomic.LoadInt32(&ext.calls); got != attempts+1 {
		t.Fatalf("after the reset the extractor was called %d times, want %d", got, attempts+1)
	}
}

// los19cSucceedingExtractor succeeds without calling an LLM.
type los19cSucceedingExtractor struct{ calls int32 }

func (e *los19cSucceedingExtractor) ExtractFromSession(string, []models.Message) ([]models.Learning, error) {
	return nil, nil
}

func (e *los19cSucceedingExtractor) ExtractAndStore(string, string, []models.Message, bool) error {
	atomic.AddInt32(&e.calls, 1)
	return nil
}

func (e *los19cSucceedingExtractor) SetChunkSize(int) {}

// YM-317 Key 1: the budget counts consecutive failures. A session that succeeds
// before the limit must get it back, otherwise one earlier outage would keep
// counting against it.
func TestLos19c_SuccessRestoresTheAttemptBudget(t *testing.T) {
	s := mustStore(t)
	sess := los19cSessionWithMessages(t, s, "los19c-recover")
	readAttempts := func() int {
		t.Helper()
		var stored int
		if err := s.DB().QueryRow(`SELECT extraction_attempts FROM sessions WHERE id = ?`, sess.ID).Scan(&stored); err != nil {
			t.Fatalf("read extraction_attempts: %v", err)
		}
		return stored
	}

	// One attempt short of the limit, then a working extraction.
	failing := &los19cFailingExtractor{}
	for i := 0; i < storage.ExtractionAttemptLimit-1; i++ {
		RunExtractionPhase(failing, s, []models.Session{sess}, &config.Config{}, nil)
	}
	if got := readAttempts(); got != storage.ExtractionAttemptLimit-1 {
		t.Fatalf("extraction_attempts = %d, want %d", got, storage.ExtractionAttemptLimit-1)
	}

	ok := &los19cSucceedingExtractor{}
	RunExtractionPhase(ok, s, []models.Session{sess}, &config.Config{}, nil)
	if got := atomic.LoadInt32(&ok.calls); got != 1 {
		t.Fatalf("the successful extractor was called %d times, want 1", got)
	}
	if got := readAttempts(); got != 0 {
		t.Fatalf("a successful extraction left extraction_attempts at %d, want 0", got)
	}

	// One later failure must leave it far from the limit.
	RunExtractionPhase(failing, s, []models.Session{sess}, &config.Config{}, nil)
	if got := readAttempts(); got != 1 {
		t.Fatalf("extraction_attempts = %d after one failure past a success, want 1", got)
	}
}

// YM-317 Key 2: the apiDown breaker exists to stop a cycle when the transport is
// failing. An answer that arrives but does not parse proves the API is up, so it
// must not trip the breaker: with six parse failures in a row every session still
// has to be attempted.
func TestLos19c_ParseFailuresDoNotTripTheAPIBreaker(t *testing.T) {
	s := mustStore(t)
	client := &los19cGarbageClient{}
	ext := extraction.NewExtractor(client, s)

	var sessions []models.Session
	for i := 0; i < 6; i++ {
		id := "los19c-parse-" + string(rune('a'+i))
		sessions = append(sessions, los19cSessionWithMessages(t, s, id))
	}

	RunExtractionPhase(ext, s, sessions, &config.Config{}, client)

	if got := atomic.LoadInt32(&client.calls); got != 6 {
		t.Fatalf("only %d of 6 sessions were attempted; a parse failure was counted as an API outage", got)
	}
}

// YM-317 Key 2, the other side: transport failures must still trip the breaker,
// otherwise a cycle against a dead API would grind through the whole queue.
func TestLos19c_TransportFailuresStillTripTheAPIBreaker(t *testing.T) {
	s := mustStore(t)
	client := &los19cDownClient{}
	ext := extraction.NewExtractor(client, s)

	var sessions []models.Session
	for i := 0; i < 6; i++ {
		id := "los19c-down-" + string(rune('a'+i))
		sessions = append(sessions, los19cSessionWithMessages(t, s, id))
	}

	RunExtractionPhase(ext, s, sessions, &config.Config{}, client)

	if got := atomic.LoadInt32(&client.calls); got != 5 {
		t.Fatalf("transport failures called the API %d times, want the breaker to stop after 5", got)
	}
}

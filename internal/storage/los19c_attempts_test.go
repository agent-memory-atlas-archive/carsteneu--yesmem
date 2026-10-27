package storage

import (
	"testing"
	"time"
)

// YM-317 Key 1: the retry budget counts consecutive failures, so a session that
// grows after the batch path gave up must not stay closed. The indexer reopens the
// session when it grows; the budget has to be reopened with it.
func TestLos19c_GrownSessionGetsAFreshAttemptBudget(t *testing.T) {
	s := mustOpen(t)
	const id = "sess-lc-grown"
	seedShortSession(t, s, id, 2, 72*time.Hour)

	readAttempts := func() int {
		t.Helper()
		var attempts int
		if err := s.DB().QueryRow(`SELECT extraction_attempts FROM sessions WHERE id = ?`, id).Scan(&attempts); err != nil {
			t.Fatalf("read extraction_attempts: %v", err)
		}
		return attempts
	}

	// The session was extracted once, then failed on every reopening attempt.
	if err := s.MarkSessionExtracted(id); err != nil {
		t.Fatalf("mark session extracted: %v", err)
	}
	for i := 0; i < ExtractionAttemptLimit; i++ {
		if _, err := s.RecordExtractionAttempt(id); err != nil {
			t.Fatalf("record attempt %d: %v", i+1, err)
		}
	}
	if !s.ExtractionAttemptsExhausted(id) {
		t.Fatalf("session must be exhausted at %d attempts, has %d", ExtractionAttemptLimit, readAttempts())
	}

	// The session grows past the short-session bar: the indexer reopens it.
	seedShortSession(t, s, id, ShortSessionMaxMessages+10, 72*time.Hour)

	stored, err := s.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !stored.ExtractedAt.IsZero() {
		t.Fatalf("a grown session must be reopened, extracted_at is still %v", stored.ExtractedAt)
	}
	if got := readAttempts(); got != 0 {
		t.Fatalf("a reopened session kept extraction_attempts = %d, want 0", got)
	}
	if s.ExtractionAttemptsExhausted(id) {
		t.Fatal("a reopened session still counts as exhausted")
	}
}

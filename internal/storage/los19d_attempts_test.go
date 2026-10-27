package storage

import (
	"testing"
	"time"
)

// YM-362 review finding I1: a partially extracted session keeps extracted_at
// empty, so the old growth reset (it required extracted_at IS NOT NULL) never
// fired. Five flaky chunks exhausted the budget for good, and everything the
// session gained afterwards was never extracted.
func TestLos19d_GrownPartialSessionGetsAFreshAttemptBudget(t *testing.T) {
	s := mustOpen(t)
	const id = "sess-los19d-partial-grown"
	seedShortSession(t, s, id, 100, 72*time.Hour)

	for i := 0; i < ExtractionAttemptLimit; i++ {
		if _, err := s.RecordExtractionAttempt(id); err != nil {
			t.Fatalf("record attempt %d: %v", i+1, err)
		}
	}
	if !s.ExtractionAttemptsExhausted(id) {
		t.Fatalf("session must be exhausted at %d attempts", ExtractionAttemptLimit)
	}

	// The session grows. A partial pass never set extracted_at, it stays empty.
	seedShortSession(t, s, id, 120, 72*time.Hour)

	stored, err := s.GetSession(id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !stored.ExtractedAt.IsZero() {
		t.Fatalf("a partial session must stay unextracted, extracted_at is %v", stored.ExtractedAt)
	}
	if s.ExtractionAttemptsExhausted(id) {
		t.Fatal("a grown, partially extracted session kept its exhausted budget")
	}
}

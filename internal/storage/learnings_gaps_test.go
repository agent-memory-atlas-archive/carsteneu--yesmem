package storage

import (
	"testing"
	"time"
)

// The daemon decides on every start whether a gap review is due by comparing
// GetLastGapReviewTime against now (daemon.go catch-up branch). reviewed_at is
// written by SQLite CURRENT_TIMESTAMP, so an unparsable format makes the last
// review always read as zero and triggers an extra LLM review after every restart.
func TestGetLastGapReviewTimeReadsSQLiteTimestamp(t *testing.T) {
	s := mustOpen(t)

	if err := s.TrackGap("missing topic", "proj"); err != nil {
		t.Fatalf("track gap: %v", err)
	}
	gaps, err := s.GetActiveGaps("proj", 10)
	if err != nil {
		t.Fatalf("get active gaps: %v", err)
	}
	if len(gaps) != 1 {
		t.Fatalf("expected 1 gap, got %d", len(gaps))
	}
	if err := s.MarkGapReviewed(gaps[0].ID, "keep"); err != nil {
		t.Fatalf("mark gap reviewed: %v", err)
	}

	last := s.GetLastGapReviewTime()
	if last.IsZero() {
		t.Fatal("GetLastGapReviewTime returned the zero time — reviewed_at was not parsed")
	}
	if age := time.Since(last); age < 0 || age > 2*time.Minute {
		t.Fatalf("last review time %v is not recent (age %v)", last, age)
	}
}

// first_seen/last_seen go through the same parseTime path as reviewed_at.
func TestGapTimestampsAreParsed(t *testing.T) {
	s := mustOpen(t)

	if err := s.TrackGap("missing topic", "proj"); err != nil {
		t.Fatalf("track gap: %v", err)
	}
	gaps, err := s.GetActiveGaps("proj", 10)
	if err != nil {
		t.Fatalf("get active gaps: %v", err)
	}
	if len(gaps) != 1 {
		t.Fatalf("expected 1 gap, got %d", len(gaps))
	}
	if gaps[0].FirstSeen.IsZero() {
		t.Error("first_seen was not parsed")
	}
	if gaps[0].LastSeen.IsZero() {
		t.Error("last_seen was not parsed")
	}
}

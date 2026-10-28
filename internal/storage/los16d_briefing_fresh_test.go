package storage

import (
	"testing"
	"time"
)

// TestCountFreshRefinedBriefingsSkipsExpiredLos16d (YM-372 [BRF-B14], Befund 2):
// the setup status printed the row count of refined_briefings. A row past the TTL
// is not served by GetRefinedBriefing or GetRefinedBriefingHash, so it must not be
// reported as a cached briefing either.
func TestCountFreshRefinedBriefingsSkipsExpiredLos16d(t *testing.T) {
	s := mustOpen(t)

	if err := s.SaveRefinedBriefing("/los16d/fresh", "hash-fresh", "text", "test"); err != nil {
		t.Fatalf("save fresh briefing: %v", err)
	}
	if err := s.SaveRefinedBriefing("/los16d/expired", "hash-expired", "text", "test"); err != nil {
		t.Fatalf("save expiring briefing: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE refined_briefings SET generated_at = ? WHERE project = ?`,
		time.Now().Add(-(refinedBriefingTTL + time.Hour)).UTC().Format(time.RFC3339), "/los16d/expired"); err != nil {
		t.Fatalf("expire briefing: %v", err)
	}

	// Precondition: the read path already treats the old row as absent.
	text, err := s.GetRefinedBriefing("/los16d/expired")
	if err != nil {
		t.Fatalf("read expired briefing: %v", err)
	}
	if text != "" {
		t.Fatalf("precondition: expired briefing must read as a miss, got %q", text)
	}

	got, err := s.CountFreshRefinedBriefings()
	if err != nil {
		t.Fatalf("count fresh briefings: %v", err)
	}
	if got != 1 {
		t.Errorf("only the fresh briefing may count, got %d", got)
	}
}

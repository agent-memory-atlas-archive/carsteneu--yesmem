package storage

import (
	"testing"
	"time"
)

// TestRefinedBriefingCacheExpiresLos16c (YM-343 [BRF-B13], Befund 2): refine.go
// claimed a TTL but no read path checked generated_at, so a briefing written once
// was served forever. The TTL must also silence GetRefinedBriefingHash, or the
// refresh loop (daemon.go, fingerprint vs cached hash) never regenerates the
// expired row.
func TestRefinedBriefingCacheExpiresLos16c(t *testing.T) {
	s := mustOpen(t)

	const project = "los16c-expiry"
	if err := s.SaveRefinedBriefing(project, "hash-fresh", "refined prose", "test-model"); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.GetRefinedBriefing(project)
	if err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if got != "refined prose" {
		t.Fatalf("fresh briefing must be served, got %q", got)
	}
	hash, err := s.GetRefinedBriefingHash(project)
	if err != nil {
		t.Fatalf("get fresh hash: %v", err)
	}
	if hash != "hash-fresh" {
		t.Fatalf("fresh hash must be returned, got %q", hash)
	}

	// Age the row beyond the TTL, which mirrors the daemon's 24h refresh cadence.
	old := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE refined_briefings SET generated_at = ? WHERE project = ?`, old, project); err != nil {
		t.Fatalf("age row: %v", err)
	}

	if got, err := s.GetRefinedBriefing(project); err != nil {
		t.Fatalf("get expired: %v", err)
	} else if got != "" {
		t.Errorf("expired briefing must not be served, got %q", got)
	}
	if hash, err := s.GetRefinedBriefingHash(project); err != nil {
		t.Fatalf("get expired hash: %v", err)
	} else if hash != "" {
		t.Errorf("expired briefing hash must read empty so the refresh loop regenerates, got %q", hash)
	}
}

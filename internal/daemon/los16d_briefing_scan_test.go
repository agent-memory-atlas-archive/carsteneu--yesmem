package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
)

// TestBriefingRefreshScanSurvivesOneUnreadableFingerprintLos16d (YM-372, Review-Fix
// zu Befund 4): once the fingerprint reports its read errors, an abort inside the
// per-project loop would cost every healthy project as well, and the next attempt is
// a 24h tick away. The scan skips the project that failed and keeps the rest.
func TestBriefingRefreshScanSurvivesOneUnreadableFingerprintLos16d(t *testing.T) {
	s := mustStore(t)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	insertSessionRow(t, s, "sess-los16d-scan", cwd, time.Now().Add(-time.Hour).Format(time.RFC3339))
	if err := s.SaveRefinedBriefing(cwd, "stale-hash", "old prose", "opus"); err != nil {
		t.Fatalf("save refined briefing: %v", err)
	}

	// The learnings read is the first one that can fail on its own while the
	// sessions half and the project list stay readable.
	if _, err := s.DB().Exec(`DROP TABLE learnings`); err != nil {
		t.Fatalf("drop learnings: %v", err)
	}

	targets, err := listProjectsNeedingBriefingRefresh(s, &config.Config{})
	if err != nil {
		t.Fatalf("an unreadable project must not abort the whole scan: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("a project whose fingerprint failed must be skipped, got %d targets", len(targets))
	}
}

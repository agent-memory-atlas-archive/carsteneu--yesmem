package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/codescan"
)

// The sweep deletes an absolute scan key only when its directory is gone. A live
// repository without any session keeps its row: a missing session is not
// evidence of a dead project, and a wrong deletion would cost a re-scan of a
// repository that is still there (YM-311: the package doc claimed otherwise).
func TestPruneOrphanedScanState_KeepsRepoWithoutSession(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// One live session elsewhere, so the sweep is not disarmed by an empty
	// live set.
	insertScanPruneSession(t, s, t.TempDir())

	repo := t.TempDir()
	key := codescan.ScanCacheKey(repo)
	if err := s.SaveProjectScan(&ProjectScanRow{Project: key, ScanJSON: `{"Files":[]}`, GitHead: "h"}); err != nil {
		t.Fatalf("save scan: %v", err)
	}

	scans, _, err := s.PruneOrphanedScanState(24*time.Hour, codescan.ScanCacheKey)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if scans != 0 {
		t.Errorf("pruned %d scan row(s), want 0 (the repository still exists)", scans)
	}
	if row, _ := s.GetProjectScan(key); row == nil {
		t.Errorf("scan row %q of an existing repository was pruned although no session names it", key)
	}
}

package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/codescan"
)

func insertScanPruneSession(t *testing.T, s *Store, projectPath string) {
	t.Helper()
	// Production shape since v0.65: project_short holds the absolute path.
	if _, err := s.db.Exec(
		`INSERT INTO sessions (id, project, project_short, started_at, jsonl_path, indexed_at)
		 VALUES (?, ?, ?, ?, '', ?)`,
		"sess-"+projectPath, projectPath, projectPath, "2026-10-01T10:00:00Z", "2026-10-01T10:00:00Z",
	); err != nil {
		t.Fatalf("insert session %s: %v", projectPath, err)
	}
}

// YM-201: orphaned scan bookkeeping (dead worktrees, projects without any
// session, expired cooldowns) was never cleaned up. The scan cache is keyed by
// the repository root, sessions by the absolute path.
func TestPruneOrphanedScanState(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	live := t.TempDir()
	dead := t.TempDir()
	// Resolve both keys while the directories still exist.
	liveKey, deadKey := codescan.ScanCacheKey(live), codescan.ScanCacheKey(dead)
	insertScanPruneSession(t, s, live)
	insertScanPruneSession(t, s, dead)
	if err := os.RemoveAll(dead); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{liveKey, deadKey, "legacyproj"} {
		if err := s.SaveProjectScan(&ProjectScanRow{Project: p, ScanJSON: `{"Files":[]}`, GitHead: "h"}); err != nil {
			t.Fatalf("save scan %s: %v", p, err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO scan_cooldown (project, failed_at, error) VALUES (?, ?, 'fresh')`,
		live, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO scan_cooldown (project, failed_at, error) VALUES (?, ?, 'old')`,
		"oldname", time.Now().Add(-30*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	scans, cooldowns, err := s.PruneOrphanedScanState(24*time.Hour, codescan.ScanCacheKey)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if scans != 2 {
		t.Errorf("pruned scans = %d, want 2 (deadproj, legacyproj)", scans)
	}
	if cooldowns != 1 {
		t.Errorf("pruned cooldowns = %d, want 1 (oldname)", cooldowns)
	}

	if row, err := s.GetProjectScan(liveKey); err != nil || row == nil {
		t.Errorf("live project scan was pruned: key=%s row=%v err=%v", liveKey, row, err)
	}
	for _, p := range []string{deadKey, "legacyproj"} {
		if row, _ := s.GetProjectScan(p); row != nil {
			t.Errorf("%s scan row survived the prune", p)
		}
	}
	var files int
	if err := s.db.QueryRow(`SELECT count(*) FROM project_scan_files WHERE project = ?`, deadKey).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Errorf("project_scan_files for %s survived: %d rows", deadKey, files)
	}
	if failedAt, _, _ := s.GetScanFailure(live); failedAt.IsZero() {
		t.Error("fresh cooldown row was pruned")
	}
	if failedAt, _, _ := s.GetScanFailure("oldname"); !failedAt.IsZero() {
		t.Error("expired cooldown row survived the prune")
	}
}

// A session can sit in a subdirectory of the repository. The scan cache key is
// the repository root, not the basename of the session directory.
func TestPruneOrphanedScanState_KeepsRepoKeyForSubdirSession(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// ScanCacheKey answers with the resolved repository root, and macOS reaches
	// its temp root through /tmp -> /private/tmp.
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve repo: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "internal", "storage")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	insertScanPruneSession(t, s, sub)

	key := codescan.ScanCacheKey(sub)
	if key != repo {
		t.Fatalf("scan key %q, want the repository root %q", key, repo)
	}
	if err := s.SaveProjectScan(&ProjectScanRow{Project: key, ScanJSON: `{"Files":[]}`, GitHead: "h"}); err != nil {
		t.Fatalf("save scan: %v", err)
	}

	scans, _, err := s.PruneOrphanedScanState(24*time.Hour, codescan.ScanCacheKey)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if scans != 0 {
		t.Errorf("pruned %d rows, want 0 (key %q belongs to a live repo)", scans, key)
	}
	if row, _ := s.GetProjectScan(key); row == nil {
		t.Errorf("live repo scan row %q was pruned", key)
	}
}

// Without a single resolvable session path the sweep cannot tell an orphan from
// a live project and must leave the cache alone.
func TestPruneOrphanedScanState_EmptyLiveSetIsNoOp(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.SaveProjectScan(&ProjectScanRow{Project: "ghostproj", ScanJSON: `{"Files":[]}`, GitHead: "h"}); err != nil {
		t.Fatalf("save scan: %v", err)
	}
	scans, _, err := s.PruneOrphanedScanState(24*time.Hour, codescan.ScanCacheKey)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if scans != 0 {
		t.Errorf("pruned %d rows without any live session, want 0", scans)
	}
	if row, _ := s.GetProjectScan("ghostproj"); row == nil {
		t.Error("scan row pruned although the live set was empty")
	}
}

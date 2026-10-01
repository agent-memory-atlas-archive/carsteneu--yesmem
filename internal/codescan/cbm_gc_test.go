package codescan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gcFixture builds a fake CBM cache directory plus a repo base dir without
// dashes in its path (keeps slug reconstruction unambiguous).
func gcFixture(t *testing.T) (cacheDir, baseDir string) {
	t.Helper()
	cacheDir = t.TempDir()
	baseDir = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CBM_LOG", filepath.Join(cacheDir, "fake-calls.log"))
	return cacheDir, baseDir
}

func writeCacheDB(t *testing.T, cacheDir, slug string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(cacheDir, slug+".db")
	if err := os.WriteFile(path, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if age != 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// Claude Code worktree DBs (<repo>/.claude/worktrees/<name> → marker
// "-.claude-worktrees-") follow the same eligibility rules as yesloop
// worktree DBs.
func TestCBMOrphanedDBsClaudeMarker(t *testing.T) {
	cacheDir, baseDir := gcFixture(t)
	baseSlug := strings.ReplaceAll(strings.TrimPrefix(baseDir, "/"), "/", "-")

	activePath := filepath.Join(baseDir, ".claude", "worktrees", "cc-active")
	if err := os.MkdirAll(activePath, 0o755); err != nil {
		t.Fatal(err)
	}
	activeDB := writeCacheDB(t, cacheDir, baseSlug+"-.claude-worktrees-cc-active", 26*time.Hour)
	orphanDB := writeCacheDB(t, cacheDir, baseSlug+"-.claude-worktrees-cc-gone", 26*time.Hour)
	t.Setenv("FAKE_CBM_ROOTPATH", "/definitely-missing-cbm-root")
	bin, _ := writeFakeCBM(t)

	findings, err := cbmOrphanedDBs(cacheDir, time.Now(), bin)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range findings {
		got[f.Path] = f.Reason
	}
	if _, ok := got[orphanDB]; !ok {
		t.Fatalf("expected finding for claude-worktrees orphan %s, got %v", orphanDB, got)
	}
	if _, ok := got[activeDB]; ok {
		t.Errorf("active claude worktree must not be flagged")
	}
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func TestCBMOrphanedDBs(t *testing.T) {
	cacheDir, baseDir := gcFixture(t)
	t.Setenv("FAKE_CBM_ROOTPATH", "/definitely-missing-cbm-root")
	bin, _ := writeFakeCBM(t)

	baseSlug := strings.ReplaceAll(strings.TrimPrefix(baseDir, "/"), "/", "-")
	if !dirExists(baseDir) || strings.Contains(baseSlug, "--") {
		t.Fatalf("fixture path assumption broken: %s", baseSlug)
	}

	// active worktree: dir exists -> keep
	activePath := filepath.Join(baseDir, ".worktrees", "wt-active")
	if err := os.MkdirAll(activePath, 0o755); err != nil {
		t.Fatal(err)
	}
	activeDB := writeCacheDB(t, cacheDir, baseSlug+"-.worktrees-wt-active", 26*time.Hour)

	// old orphan -> delete
	orphanDB := writeCacheDB(t, cacheDir, baseSlug+"-.worktrees-wt-gone", 26*time.Hour)
	orphanWal := orphanDB + "-wal"
	if err := os.WriteFile(orphanWal, []byte("wal"), 0o644); err != nil {
		t.Fatal(err)
	}

	// fresh orphan -> keep (mtime < 24h)
	freshDB := writeCacheDB(t, cacheDir, baseSlug+"-.worktrees-wt-fresh", 1*time.Hour)

	// unresolvable base -> keep (never delete what we cannot verify)
	unresolvable := writeCacheDB(t, cacheDir, "no-such-nonexistent-repo-.worktrees-wt", 26*time.Hour)

	// main-repo DB (no -.worktrees- marker) -> keep
	mainDB := writeCacheDB(t, cacheDir, baseSlug, 26*time.Hour)

	// stage leftover for the orphan
	stage := filepath.Join(cacheDir, baseSlug+"-.worktrees-wt-gone.db.stage.abc")
	if err := os.WriteFile(stage, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings, err := cbmOrphanedDBs(cacheDir, time.Now(), bin)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, f := range findings {
		got[f.Path] = f.Reason
	}

	for _, want := range []string{orphanDB, orphanWal, stage} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected finding for %s, findings: %v", want, got)
		}
	}
	for _, keep := range []string{activeDB, freshDB, unresolvable, mainDB} {
		if _, ok := got[keep]; ok {
			t.Errorf("unexpected finding for %s", keep)
		}
	}
	if len(got) != 3 {
		t.Errorf("expected exactly 3 findings, got %d", len(got))
	}
}

func TestRunCBMGCDryRunKeepsFiles(t *testing.T) {
	cacheDir, baseDir := gcFixture(t)
	baseSlug := strings.ReplaceAll(strings.TrimPrefix(baseDir, "/"), "/", "-")
	db := writeCacheDB(t, cacheDir, baseSlug+"-.worktrees-wt-gone", 26*time.Hour)
	wal := db + "-wal"
	if err := os.WriteFile(wal, []byte("wal"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CBM_ROOTPATH", "/definitely-missing-cbm-root")
	bin, _ := writeFakeCBM(t)

	findings, err := cbmOrphanedDBs(cacheDir, time.Now(), bin)
	if err != nil || len(findings) != 2 {
		t.Fatalf("expected 2 findings (db+wal), got %v (%v)", findings, err)
	}

	runCBMGCGated(findings, true)

	if _, err := os.Stat(db); err != nil {
		t.Fatal("dry-run deleted the DB")
	}
	if _, err := os.Stat(wal); err != nil {
		t.Fatal("dry-run deleted the WAL")
	}
}

func TestRunCBMGCDeletesFindings(t *testing.T) {
	cacheDir, baseDir := gcFixture(t)
	baseSlug := strings.ReplaceAll(strings.TrimPrefix(baseDir, "/"), "/", "-")
	db := writeCacheDB(t, cacheDir, baseSlug+"-.worktrees-wt-gone", 26*time.Hour)
	wal := db + "-wal"
	if err := os.WriteFile(wal, []byte("wal"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CBM_ROOTPATH", "/definitely-missing-cbm-root")
	bin, _ := writeFakeCBM(t)

	findings, err := cbmOrphanedDBs(cacheDir, time.Now(), bin)
	if err != nil || len(findings) != 2 {
		t.Fatalf("expected 2 findings (db+wal), got %v (%v)", findings, err)
	}

	runCBMGCGated(findings, false)

	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("real run did not delete the DB")
	}
	if _, err := os.Stat(wal); !os.IsNotExist(err) {
		t.Fatal("real run did not delete the WAL")
	}
}

// A DB whose CBM-registered root path still exists is never a GC candidate,
// even if its slug looks like an orphaned worktree (marker lookalike).
func TestCBMGCSkipsWhenRegisteredRootExists(t *testing.T) {
	cacheDir, baseDir := gcFixture(t)
	baseSlug := strings.ReplaceAll(strings.TrimPrefix(baseDir, "/"), "/", "-")
	db := writeCacheDB(t, cacheDir, baseSlug+"-.worktrees-wt-gone", 26*time.Hour)
	t.Setenv("FAKE_CBM_ROOTPATH", baseDir) // registered root exists
	bin, _ := writeFakeCBM(t)

	findings, err := cbmOrphanedDBs(cacheDir, time.Now(), bin)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("registered existing root must suppress findings, got %v", findings)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatal("DB vanished although registered root exists")
	}
}

// A DB whose registered root path is unknown must never be deleted.
func TestCBMGCSkipsWhenRootPathUnknown(t *testing.T) {
	cacheDir, _ := gcFixture(t)
	db := writeCacheDB(t, cacheDir, "lookalike-repo-x-.worktrees-wt", 26*time.Hour)
	t.Setenv("FAKE_CBM_ROOTPATH", "")
	bin, _ := writeFakeCBM(t)

	findings, err := cbmOrphanedDBs(cacheDir, time.Now(), bin)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("unknown root path must suppress findings, got %v", findings)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatal("DB vanished although root path unknown")
	}
}

// An unstat-able worktree path (EACCES etc.) is not proof that the worktree
// is gone — the DB must be kept.
func TestCBMGCSkipsOnUnreadableWorktreePath(t *testing.T) {
	cacheDir, baseDir := gcFixture(t)
	baseSlug := strings.ReplaceAll(strings.TrimPrefix(baseDir, "/"), "/", "-")
	wtParent := filepath.Join(baseDir, ".worktrees")
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCacheDB(t, cacheDir, baseSlug+"-.worktrees-wt-gone", 26*time.Hour)
	if err := os.Chmod(wtParent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(wtParent, 0o755) })
	t.Setenv("FAKE_CBM_ROOTPATH", "/definitely-missing-cbm-root")
	bin, _ := writeFakeCBM(t)

	findings, err := cbmOrphanedDBs(cacheDir, time.Now(), bin)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("unreadable worktree path must suppress findings, got %v", findings)
	}
}

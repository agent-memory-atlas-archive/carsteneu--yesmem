package codescan

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// isolateCBMCache redirects the CBM cache directory (HOME override) to a
// test-local dir and returns the redirected cache path.
func isolateCBMCache(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".cache", "codebase-memory-mcp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func tempGitRepoWithCommit(t *testing.T) string {
	t.Helper()
	root := tempGitRepo(t)
	cmd := exec.Command("git", "-C", root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-qm", "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	return root
}

func fakeIndexDB(t *testing.T, cacheDir, root string, mtime time.Time) string {
	t.Helper()
	db := filepath.Join(cacheDir, cbmProjectName(root)+".db")
	if err := os.WriteFile(db, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(db, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return db
}

// A stale index (CBM mtime older than HEAD commit time) triggers exactly one
// background refresh per HEAD; a new HEAD re-arms the guard.
func TestEnsureIndexIfStale_TriggersOncePerHead(t *testing.T) {
	root := tempGitRepoWithCommit(t)
	cacheDir := isolateCBMCache(t)
	db := fakeIndexDB(t, cacheDir, root, time.Now().Add(-2*time.Hour))
	_ = db

	var mu sync.Mutex
	calls := 0
	orig := ensureTrigger
	ensureTrigger = func(dir string) { mu.Lock(); calls++; mu.Unlock() }
	t.Cleanup(func() { ensureTrigger = orig })
	t.Cleanup(func() {
		indexFlightMu.Lock()
		attemptedRefresh = map[string]string{}
		indexFlightMu.Unlock()
	})

	EnsureIndexIfStale(root, "h1")
	EnsureIndexIfStale(root, "h1")
	mu.Lock()
	if calls != 1 {
		t.Fatalf("expected 1 trigger for repeated same-HEAD calls, got %d", calls)
	}
	mu.Unlock()

	EnsureIndexIfStale(root, "h2")
	mu.Lock()
	if calls != 2 {
		t.Fatalf("expected a new HEAD to re-arm the guard, got %d triggers", calls)
	}
	mu.Unlock()
}

// A CBM index mtime NEWER than the HEAD commit time means the index already
// reflects the current commit — no background refresh.
func TestEnsureIndexIfStale_FreshIndexNoTrigger(t *testing.T) {
	root := tempGitRepoWithCommit(t)
	cacheDir := isolateCBMCache(t)
	db := filepath.Join(cacheDir, cbmProjectName(root)+".db")
	if err := os.WriteFile(db, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(db, future, future); err != nil {
		t.Fatal(err)
	}

	EnsureIndexIfStale(root, gitHeadOrFatal(t, root))

	indexFlightMu.Lock()
	defer indexFlightMu.Unlock()
	if n := len(attemptedRefresh); n != 0 {
		t.Fatalf("fresh index must not trigger refresh, recorded %d", n)
	}
}

func gitHeadOrFatal(t *testing.T, root string) string {
	t.Helper()
	head := ReadGitHead(root)
	if head == "" {
		t.Fatal("no HEAD in test repo")
	}
	return head
}

// Layer 1 must invalidate when the CBM index mtime changes, so a completed
// background reindex becomes visible without a new commit.
func TestCachedScannerInvalidatesOnMtimeChange(t *testing.T) {
	root := tempGitRepoWithCommit(t)
	cacheDir := isolateCBMCache(t)
	db := fakeIndexDB(t, cacheDir, root, time.Now())

	scans := 0
	inner := &stubScanner{fn: func(dir string) (*ScanResult, error) {
		scans++
		return &ScanResult{}, nil
	}}
	cs := NewCachedScanner(inner)

	if _, err := cs.Scan(root); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Scan(root); err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Fatalf("expected Layer-1 hit on identical HEAD+mtime, ran %d scans", scans)
	}

	// Simulate a completed background reindex touching the index DB.
	newer := time.Now().Add(3 * time.Hour)
	if err := os.Chtimes(db, newer, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Scan(root); err != nil {
		t.Fatal(err)
	}
	if scans != 2 {
		t.Fatalf("expected Layer-1 miss after mtime change, ran %d scans", scans)
	}
}

// Scan must offer the staleness refresh on the success path.
func TestScanCallsStaleRefresh(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAKE_CBM_LOG", filepath.Join(tmp, "fake-calls.log"))
	t.Setenv("FAKE_CBM_BEHAVIOR", "ok")
	bin, _ := writeFakeCBM(t)
	root := tempGitRepoWithCommit(t)

	var got string
	s := NewCBMScanner()
	s.bin = bin
	s.ensureIndexIfStale = func(dir, head string) { got = head }
	if _, err := s.Scan(root); err != nil {
		t.Fatal(err)
	}
	if head := gitHeadOrFatal(t, root); got != head {
		t.Fatalf("expected stale refresh with HEAD %s, got %q", head, got)
	}
}

type stubScanner struct {
	fn func(string) (*ScanResult, error)
}

func (s *stubScanner) Scan(dir string) (*ScanResult, error) { return s.fn(dir) }

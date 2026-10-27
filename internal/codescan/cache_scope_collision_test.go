package codescan

import (
	"os"
	"path/filepath"
	"testing"
)

// los04eRow is one persisted scan row.
type los04eRow struct {
	scanJSON string
	gitHead  string
	cbmMtime int64
}

// los04eRowStore implements ScanStore in memory, one row per project key.
type los04eRowStore struct {
	rows map[string]los04eRow
}

func (s *los04eRowStore) LoadScan(project string) (string, string, int64, error) {
	r, ok := s.rows[project]
	if !ok {
		return "", "", 0, nil
	}
	return r.scanJSON, r.gitHead, r.cbmMtime, nil
}

func (s *los04eRowStore) PersistScan(project, scanJSON, gitHead string, cbmMtime int64) error {
	s.rows[project] = los04eRow{scanJSON: scanJSON, gitHead: gitHead, cbmMtime: cbmMtime}
	return nil
}

// los04eRepoAt builds a repository of the given name below parent and hands it
// the given git head. Two repositories built with the same head stand for two
// clones of the same commit in differently named parent directories: same
// basename, same head, different repository.
func los04eRepoAt(t *testing.T, parent, name, head string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	gitDir := filepath.Join(repo, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "refs", "heads", "main"), []byte(head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// Two repositories with the same basename must not share one scan cache row:
// with a shared key and a matching git head the second repository is served the
// first repository's file list, and whichever scans last overwrites the other's
// index (YM-277 / DEV-DT45).
func TestScanCache_SameNamedReposDoNotShareTheirScanRow(t *testing.T) {
	root := t.TempDir()
	a := los04eRepoAt(t, filepath.Join(root, "work"), "yesmem", "samehead")
	b := los04eRepoAt(t, filepath.Join(root, "home"), "yesmem", "samehead")

	// The display name is the repository name and stays ambiguous by design.
	if ProjectKey(a) != ProjectKey(b) {
		t.Fatalf("fixture broken: display names differ (%q vs %q)", ProjectKey(a), ProjectKey(b))
	}

	store := &los04eRowStore{rows: map[string]los04eRow{}}

	resA, err := NewCachedScanner(&countingScanner{}).WithStore(store).Scan(a)
	if err != nil {
		t.Fatalf("scan a: %v", err)
	}
	if resA.RootDir != a {
		t.Fatalf("scan a returned root %q, want %q", resA.RootDir, a)
	}

	resB, err := NewCachedScanner(&countingScanner{}).WithStore(store).Scan(b)
	if err != nil {
		t.Fatalf("scan b: %v", err)
	}
	if resB.RootDir != b {
		t.Fatalf("repo b was served the scan of %q", resB.RootDir)
	}
	if len(store.rows) != 2 {
		t.Fatalf("scan cache holds %d row(s) for two repositories, want 2: %v", len(store.rows), store.rows)
	}
}

// The two keys the collision fix separates must stay separate: the display name
// keeps the repository name, the storage key carries the checkout path. A linked
// worktree is a checkout of its own and keeps its path as its key (YM-453).
func TestScanCacheKey_DisplayNameStaysBasename(t *testing.T) {
	root := t.TempDir()
	repo := los04eRepoAt(t, root, "yesmem", "head1")
	sub := filepath.Join(repo, "internal", "proxy")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := ProjectKey(repo); got != "yesmem" {
		t.Fatalf("ProjectKey = %q, want the repository name %q", got, "yesmem")
	}
	if got := ScanCacheKey(repo); got != repo {
		t.Fatalf("ScanCacheKey = %q, want the repository root %q", got, repo)
	}
	if got := ScanCacheKey(sub); got != repo {
		t.Fatalf("ScanCacheKey below the root = %q, want %q", got, repo)
	}

	// A linked worktree is a checkout of its own: its git data lives in the main
	// repository, but the tree it scans is another one (YM-453).
	wtGitDir := filepath.Join(repo, ".git", "worktrees", "wt")
	if err := os.MkdirAll(wtGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "yesmem-wt")
	if err := os.MkdirAll(filepath.Join(wt, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ScanCacheKey(wt); got != wt {
		t.Fatalf("ScanCacheKey of a linked worktree = %q, want the worktree root %q", got, wt)
	}
	if got := ScanCacheKey(filepath.Join(wt, "internal")); got != wt {
		t.Fatalf("ScanCacheKey below a linked worktree = %q, want %q", got, wt)
	}
	if got := ProjectKey(wt); got != "yesmem" {
		t.Fatalf("ProjectKey of a linked worktree = %q, want the repository name %q", got, "yesmem")
	}
}

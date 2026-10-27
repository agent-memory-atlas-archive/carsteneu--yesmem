package models

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// los56hMkdir creates a directory below root and returns it.
func los56hMkdir(t *testing.T, root string, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{root}, parts...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

// TestLos56hRepositoryRootCacheFollowsANewCheckout covers YM-390(1): a positive cache
// entry was trusted for the lifetime of the process. A nested checkout that appears
// afterwards stayed invisible, so its paths kept resolving to the enclosing repository
// and their learnings were attributed to the wrong project.
func TestLos56hRepositoryRootCacheFollowsANewCheckout(t *testing.T) {
	root := t.TempDir()
	repo := los56hMkdir(t, root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo/.git: %v", err)
	}
	nested := los56hMkdir(t, root, "repo", "vendor", "lib")

	if got := RepositoryRoot(nested); got != repo {
		t.Fatalf("RepositoryRoot(%q) = %q, want the enclosing repository %q", nested, got, repo)
	}

	// The nested checkout appears after the answer was cached.
	if err := os.MkdirAll(filepath.Join(nested, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir nested/.git: %v", err)
	}
	// Age the cache beyond its trust window instead of sleeping.
	previous := repositoryRootNow
	repositoryRootNow = func() time.Time { return time.Now().Add(2 * repositoryRootTTL) }
	t.Cleanup(func() { repositoryRootNow = previous })

	if got := RepositoryRoot(nested); got != nested {
		t.Errorf("RepositoryRoot(%q) = %q after the nested checkout appeared, want %q", nested, got, nested)
	}
}

// TestLos56hRepositoryRootKeepsAFreshAnswer is the other side: within the trust window
// the cached answer is used, so the hot path does not walk the tree on every call. That
// is the bounded staleness the TTL accepts, and the test pins it deliberately.
func TestLos56hRepositoryRootKeepsAFreshAnswer(t *testing.T) {
	root := t.TempDir()
	repo := los56hMkdir(t, root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo/.git: %v", err)
	}
	sub := los56hMkdir(t, root, "repo", "pkg")

	if got := RepositoryRoot(sub); got != repo {
		t.Fatalf("RepositoryRoot(%q) = %q, want %q", sub, got, repo)
	}

	// A checkout appears, but the answer is younger than the trust window: it stays.
	if err := os.MkdirAll(filepath.Join(sub, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir sub/.git: %v", err)
	}
	if got := RepositoryRoot(sub); got != repo {
		t.Errorf("within the trust window the cached answer must be reused, got %q", got)
	}
}

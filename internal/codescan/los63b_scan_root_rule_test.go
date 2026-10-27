package codescan

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// los63bWorktree builds a main checkout with a linked worktree the way git
// creates one: the worktree root carries a .git file that points at the main
// checkout's git data.
func los63bWorktree(t *testing.T) (main, worktree string) {
	t.Helper()
	main = t.TempDir()
	steps := [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init"},
		{"worktree", "add", "-q", filepath.Join(main, ".worktrees", "los63b")},
	}
	for _, args := range steps {
		if out, err := exec.Command("git", append([]string{"-C", main}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return main, filepath.Join(main, ".worktrees", "los63b")
}

// YM-453: the scan cache key names the checkout whose file list a document
// describes. A linked worktree is a checkout of its own, so its scan must not be
// filed under the main checkout's key: that row is what the main checkout's
// index and wiki are rendered from.
func TestLos63bScanCacheKeyKeepsTheCheckout(t *testing.T) {
	main, worktree := los63bWorktree(t)

	if got := ScanCacheKey(main); got != main {
		t.Fatalf("ScanCacheKey(main) = %q, want %q", got, main)
	}
	if got := ScanCacheKey(worktree); got != worktree {
		t.Fatalf("ScanCacheKey(worktree) = %q, want the worktree root %q", got, worktree)
	}
	if got := ScanCacheKey(filepath.Join(worktree, "internal")); got != worktree {
		t.Fatalf("ScanCacheKey below the worktree = %q, want %q", got, worktree)
	}
}

// YM-453: both guards answer one question — may this document be the index the
// key names. The worktree case stays allowed, because the worktree answers for
// itself and not for the main checkout.
func TestLos63bScanRootRuleOnTheWorktree(t *testing.T) {
	main, worktree := los63bWorktree(t)

	if !ScannedRepositoryRoot(ScanCacheKey(worktree), worktree) {
		t.Error("a worktree scan is not accepted as its own index")
	}
	if ScannedRepositoryRoot(ScanCacheKey(main), worktree) {
		t.Error("a worktree scan is accepted as the main checkout's index")
	}
	if ScannedRepositoryRoot(ScanCacheKey(main), filepath.Join(main, "internal")) {
		t.Error("a subtree scan is accepted as the checkout's index")
	}
	if !ScannedRepositoryRoot(ScanCacheKey(main), main) {
		t.Error("the checkout's own scan is not accepted")
	}
}

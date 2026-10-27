package codescan

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// YM-449 / DEV-DT73: cmd_wiki_render decided on its own whether a scan is the
// repository's index, with a raw string comparison against the project key. The
// rule now lives here, canonicalized on both sides, so a symlinked project path
// and a subdirectory both answer correctly.
func TestScannedRepositoryRoot(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sub := filepath.Join(repo, "internal", "coder")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	key := ScanCacheKey(repo)
	if !ScannedRepositoryRoot(key, repo) {
		t.Errorf("a scan of the repository root is not accepted as its index")
	}
	if ScannedRepositoryRoot(key, sub) {
		t.Errorf("a scan of a subdirectory is accepted as the repository's index")
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !ScannedRepositoryRoot(key, link) {
		t.Errorf("the same root reached through a symlink is not accepted")
	}

	if ScannedRepositoryRoot("", repo) {
		t.Errorf("an empty project key accepted a scan")
	}
}

// A linked worktree keeps its own checkout: the project key folds the worktree
// onto the main repository, so a scan taken in the worktree is not the index that
// key stores (YM-277, YM-449).
func TestScannedRepositoryRoot_WorktreeIsNotTheMainIndex(t *testing.T) {
	main := t.TempDir()
	if out, err := exec.Command("git", "-C", main, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", main, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	wt := filepath.Join(main, "wt")
	if out, err := exec.Command("git", "-C", main, "worktree", "add", "-q", wt).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	if ScannedRepositoryRoot(ScanCacheKey(main), wt) {
		t.Errorf("a worktree scan is accepted as the main repository's index")
	}
}

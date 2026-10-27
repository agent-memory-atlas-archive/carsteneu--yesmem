package codescan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mainRepoWithWorktree builds a main repository plus a linked worktree. Both
// directories collapse onto the same project key (the repo basename), which is
// the constellation from DT27: a worktree scan failure used to lock the main
// repo's code index.
func mainRepoWithWorktree(t *testing.T) (mainRepo, worktree string) {
	t.Helper()
	root := realPath(t, t.TempDir())

	mainRepo = filepath.Join(root, "yesmem")
	gitDir := filepath.Join(mainRepo, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "heads"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0644)
	os.WriteFile(filepath.Join(gitDir, "refs", "heads", "main"), []byte("mainhead1\n"), 0644)

	wtGitDir := filepath.Join(gitDir, "worktrees", "wt")
	if err := os.MkdirAll(wtGitDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wtGitDir, "HEAD"), []byte("wthead22\n"), 0644)

	worktree = filepath.Join(root, "yesmem-wt")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGitDir+"\n"), 0644)
	return mainRepo, worktree
}

// A directory without its own CBM index fails its scan. That failure must stay
// scoped to the failing directory: the main repo shares the project key but
// must keep serving its code index (DT27 / YM-4).
func TestScanCooldown_ScopedToScanDirectory(t *testing.T) {
	mainRepo, worktree := mainRepoWithWorktree(t)
	if ProjectKey(mainRepo) != ProjectKey(worktree) {
		t.Fatalf("fixture broken: keys differ (%q vs %q)", ProjectKey(mainRepo), ProjectKey(worktree))
	}

	store := &memFailureStore{failures: map[string]time.Time{}}
	failing := NewCachedScanner(&countingScanner{err: errors.New("query files: cbm cli: exit status 1")}).WithStore(store)
	if _, err := failing.Scan(worktree); err == nil {
		t.Fatal("worktree scan should fail")
	}

	mainInner := &countingScanner{}
	res, err := NewCachedScanner(mainInner).WithStore(store).Scan(mainRepo)
	if err != nil {
		t.Fatalf("main repo scan was blocked by the worktree failure: %v", err)
	}
	if res == nil || res.RootDir != mainRepo {
		t.Fatalf("unexpected main repo result: %+v", res)
	}
	if mainInner.calls != 1 {
		t.Fatalf("main repo inner scan calls = %d, want 1", mainInner.calls)
	}
}

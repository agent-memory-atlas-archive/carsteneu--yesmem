package codescan

import (
	"os"
	"path/filepath"
	"testing"
)

const legacyGitdirContent = "gitdir: /somewhere/.git/worktrees/wt\n"

func setupSwapFixture(t *testing.T) (root, bak, dotGit string) {
	t.Helper()
	root = t.TempDir()
	bak = filepath.Join(root, ".git.yesmem-bak")
	dotGit = filepath.Join(root, ".git")
	if err := os.WriteFile(bak, []byte(legacyGitdirContent), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, bak, dotGit
}

// A legacy crash leftover: .git is a symlink, the original content sits in
// .git.yesmem-bak. Restore must move the content back and remove the backup.
func TestRestoreLegacyGitSwap_SymlinkedGit(t *testing.T) {
	root, bak, dotGit := setupSwapFixture(t)
	if err := os.Symlink("/nowhere/worktrees/wt/.git", dotGit); err != nil {
		t.Fatal(err)
	}

	if !RestoreLegacyGitSwap(root) {
		t.Fatal("expected restoration")
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != legacyGitdirContent {
		t.Fatalf("unexpected .git content: %q", string(data))
	}
	if _, err := os.Lstat(bak); !os.IsNotExist(err) {
		t.Fatal("backup not removed after restore")
	}
}

// Hard crash case: .git entirely missing, only the backup survives.
func TestRestoreLegacyGitSwap_MissingGit(t *testing.T) {
	root, bak, dotGit := setupSwapFixture(t)

	if !RestoreLegacyGitSwap(root) {
		t.Fatal("expected restoration")
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != legacyGitdirContent {
		t.Fatalf("unexpected .git content: %q", string(data))
	}
	if _, err := os.Lstat(bak); !os.IsNotExist(err) {
		t.Fatal("backup not removed after restore")
	}
}

// A symlink planted as .git.yesmem-bak must not be followed.
func TestRestoreLegacyGitSwap_SkipsSymlinkBackup(t *testing.T) {
	root, bak, dotGit := setupSwapFixture(t)
	if err := os.Remove(bak); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", bak); err != nil {
		t.Fatal(err)
	}

	if RestoreLegacyGitSwap(root) {
		t.Fatal("symlinked backup must not be restored")
	}
	if data, err := os.ReadFile(dotGit); err == nil {
		t.Fatalf(".git written from symlinked backup: %q", string(data))
	}
}

// A healthy worktree (.git as regular file) must stay untouched.
func TestRestoreLegacyGitSwap_HealthyUntouched(t *testing.T) {
	root, _, dotGit := setupSwapFixture(t)
	if err := os.Remove(filepath.Join(root, ".git.yesmem-bak")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dotGit, []byte(legacyGitdirContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if RestoreLegacyGitSwap(root) {
		t.Fatal("healthy worktree must not be modified")
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != legacyGitdirContent {
		t.Fatalf(".git content changed: %q", string(data))
	}
}

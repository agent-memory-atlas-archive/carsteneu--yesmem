package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	repo "github.com/carsteneu/yesmem/internal/repo"
)

func gitMain(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func gitWorktreeOf(t *testing.T, main, wt string) {
	t.Helper()
	gitDir := filepath.Join(main, ".git", "worktrees", filepath.Base(wt))
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRootPlain(t *testing.T) {
	m := t.TempDir()
	gitMain(t, m)
	if got := repo.Root(m); got != m {
		t.Errorf("Root(%q) = %q, want %q", m, got, m)
	}
}

func TestRootSubdir(t *testing.T) {
	m := t.TempDir()
	gitMain(t, m)
	sub := filepath.Join(m, "internal", "repo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := repo.Root(sub); got != m {
		t.Errorf("Root(%q) = %q, want %q", sub, got, m)
	}
}

func TestRootWorktreeResolvesToMain(t *testing.T) {
	m := t.TempDir()
	gitMain(t, m)
	wt := filepath.Join(m, ".worktrees", "feat")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitWorktreeOf(t, m, wt)
	if got := repo.Root(wt); got != m {
		t.Errorf("Root(%q) = %q, want %q", wt, got, m)
	}
}

func TestRootWorktreeAbsoluteGitDirOutsideMainTree(t *testing.T) {
	m := t.TempDir()
	gitMain(t, m)
	wtRoot := t.TempDir()
	wt := filepath.Join(wtRoot, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitWorktreeOf(t, m, wt)
	if got := repo.Root(wt); got != m {
		t.Errorf("Root(%q) = %q, want %q", wt, got, m)
	}
}

func TestRootWorktreeRelativeGitDir(t *testing.T) {
	parent := t.TempDir()
	m := filepath.Join(parent, "main")
	wt := filepath.Join(parent, "wt")
	if err := os.MkdirAll(filepath.Join(m, ".git", "worktrees", "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("..", "main", ".git", "worktrees", "wt")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := repo.Root(wt); got != m {
		t.Errorf("Root(%q) = %q, want %q", wt, got, m)
	}
}

func TestRootNoGit(t *testing.T) {
	dir := t.TempDir()
	if got := repo.Root(dir); got != "" {
		t.Errorf("Root(%q) = %q, want empty", dir, got)
	}
}

func TestRootNoGitSubdir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := repo.Root(sub); got != "" {
		t.Errorf("Root(%q) = %q, want empty", sub, got)
	}
}

func TestRootEmpty(t *testing.T) {
	if got := repo.Root(""); got != "" {
		t.Errorf("Root(\"\") = %q, want empty", got)
	}
}

func TestCloneRoot(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sub := filepath.Join(repoRoot, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	file := filepath.Join(sub, "doc.md")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{"repo root", repoRoot, repoRoot},
		{"inside repo", sub, ""},
		{"file inside repo", file, ""},
		{"plain dir", t.TempDir(), ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := repo.CloneRoot(tc.path); got != tc.want {
				t.Errorf("repo.CloneRoot(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// A worktree root is a git working tree, but pulling it would run git pull on a
// checkout a session may be working in — exactly the YM-27 damage class. Only a
// primary checkout (.git as a directory) may be pulled.
func TestCloneRoot_RejectsWorktreeRoot(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /somewhere/.git/worktrees/wt\n"), 0644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	if got := repo.CloneRoot(wt); got != "" {
		t.Errorf("repo.CloneRoot(worktree) = %q, want \"\"", got)
	}
}

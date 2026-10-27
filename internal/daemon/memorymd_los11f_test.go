package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-279 (Los 11f, aus Los 11d): the scan fallback of findProjectDir matched on
// the last dash-separated element only, so an indexed project without a Claude
// directory of its own was attributed to the first foreign directory whose name
// ended the same way. GenerateAllMemoryMDs then wrote a MEMORY.md INTO that
// foreign project (~/.claude/projects/<other>/memory/MEMORY.md).
func TestFindProjectDir_Los11f_DoesNotReachIntoAForeignProject(t *testing.T) {
	root := t.TempDir()
	foreign := filepath.Join(root, "-home-tester-projects-opencode")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("mkdir foreign project: %v", err)
	}

	if got := findProjectDir(root, "/tmp/opencode"); got != "" {
		t.Fatalf("a foreign project directory was attributed to /tmp/opencode: %q", got)
	}
}

// The base name alone is not a match either: "myproject" must not collapse into
// an unrelated "some-project".
func TestFindProjectDir_Los11f_IgnoresAPartialBaseName(t *testing.T) {
	root := t.TempDir()
	decoy := filepath.Join(root, "-home-user-some-project")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatalf("mkdir decoy: %v", err)
	}

	if got := findProjectDir(root, "/home/user/project"); got != "" {
		t.Fatalf("a partial base-name match attributed %q to /home/user/project", got)
	}
}

// The exact encoding stays the supported path, including a path that reaches the
// daemon with a trailing slash.
func TestFindProjectDir_Los11f_NormalizesATrailingSlash(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-home-user-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	if got := findProjectDir(root, "/home/user/project/"); got != dir {
		t.Fatalf("trailing slash: expected %q, got %q", dir, got)
	}
	if got := findProjectDir(root, "//home/user/project"); got != dir {
		t.Fatalf("doubled slash: expected %q, got %q", dir, got)
	}
}

// Claude Code's project directory encoding turns dots into dashes as well
// (internal/archive/archive.go ProjectDirName). Worktrees live under
// <repo>/.worktrees/<slug>, so without the dot in the encoder every loop agent
// loses its MEMORY.md.
func TestFindProjectDir_Los11f_EncodesDotsLikeClaudeCode(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-home-user-proj--worktrees-los-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	if got := findProjectDir(root, "/home/user/proj/.worktrees/los-1"); got != dir {
		t.Fatalf("dotted worktree path: expected %q, got %q", dir, got)
	}
}

// Claude's encoder also drops a "+" (verified on live data:
// .claude/worktrees/feat+capability-memory → ...-worktrees-feat-capability-memory).
func TestFindProjectDir_Los11f_EncodesPlusLikeClaudeCode(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-home-user-repo--claude-worktrees-feat-capability-memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	if got := findProjectDir(root, "/home/user/repo/.claude/worktrees/feat+capability-memory"); got != dir {
		t.Fatalf("plus in a worktree name: expected %q, got %q", dir, got)
	}
}

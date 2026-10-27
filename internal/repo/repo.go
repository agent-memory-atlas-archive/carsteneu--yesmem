// Package repo locates the main repository root for a directory.
// Worktree sessions (.git is a FILE pointing at <main>/.git/worktrees/<name>)
// resolve to the main repository, so all learnings from a worktree session
// are attributed to one project.
package repo

import (
	"os"
	"path/filepath"
	"strings"
)

// RootOrSelf returns the main repository root for an absolute directory, or
// the input unchanged when it is not an absolute path or not inside a git
// working tree. Short project names are never filesystem paths and pass
// through untouched. Used to canonicalize a caller's project before exact
// project matching, so worktree sessions match their main repo's scoped rows.
//
// It resolves to the NEAREST enclosing .git, so a nested repo (or a repo laid
// out inside another, e.g. a dotfiles repo at $HOME) collapses onto its outer
// root. That merges two project identities; in this repo every project root has
// no enclosing .git and no inner-scoped rows exist, so the collapse is inert.
func RootOrSelf(dir string) string {
	if !strings.HasPrefix(dir, "/") {
		return dir
	}
	if r := Root(dir); r != "" {
		return r
	}
	return dir
}

// git working tree. Worktrees (.git is a file with "gitdir: <main>/.git/worktrees/<name>")
// resolve to the main repository path. Limitation: gitdirs not laid out as
// <main>/.git/worktrees/<name> (bare repos, submodules) resolve to the gitdir's
// grandparent directory — best effort, see repo_test.go for covered shapes.
func Root(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		probe := filepath.Join(abs, ".git")
		if info, err := os.Stat(probe); err == nil {
			if info.IsDir() {
				return abs
			}
			if data, err := os.ReadFile(probe); err == nil {
				line := strings.TrimSpace(string(data))
				if gitDir, ok := strings.CutPrefix(line, "gitdir: "); ok {
					if !filepath.IsAbs(gitDir) {
						gitDir = filepath.Join(abs, gitDir)
					}
					return filepath.Clean(filepath.Join(gitDir, "..", "..", ".."))
				}
			}
			return abs // .git file, unparseable — best effort
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

// CloneRoot returns the absolute path when path is itself a git working tree
// root — a directory holding a .git DIRECTORY — and "" otherwise.
//
// Unlike Root it never walks up to an enclosing repository. Callers use it to
// decide whether a doc source is a standalone checkout (the docs clones under
// ~/.claude/yesmem/docs/<name>) or just a path inside the user's working repo
// (e.g. <repo>/CLAUDE.md). Pulling the latter would mean running git pull on
// the repository a session is working in.
//
// A .git FILE is rejected on purpose: that is a linked worktree (or submodule),
// a checkout a session may be actively working in — the same damage class.
func CloneRoot(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return ""
	}
	gitInfo, err := os.Stat(filepath.Join(abs, ".git"))
	if err != nil || !gitInfo.IsDir() {
		return ""
	}
	return abs
}

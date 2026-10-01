package codescan

import (
	"log"
	"os"
	"path/filepath"
)

// RestoreLegacyGitSwap repairs a worktree whose .git file was left swapped
// by an older yesmem build or a killed index call. Previous yesmem versions
// renamed the worktree's .git file to .git.yesmem-bak and symlinked .git to
// the gitdir for the duration of an index call; a daemon crash inside that
// window left the worktree without a valid .git. Restores the original
// content from the backup and reports whether a repair happened.
func RestoreLegacyGitSwap(rootDir string) bool {
	dotGit := filepath.Join(rootDir, ".git")
	bak := dotGit + ".yesmem-bak"

	if info, err := os.Lstat(dotGit); err == nil && info.Mode().IsRegular() {
		return false // healthy — nothing to do
	}

	// Lstat: only regular files — a symlink planted as .git.yesmem-bak must
	// not be followed (security hardening per round-2 review).
	info, err := os.Lstat(bak)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(bak)
	if err != nil {
		return false
	}
	_ = os.Remove(dotGit)
	if err := os.WriteFile(dotGit, data, 0o644); err != nil {
		log.Printf("cbm: git swap recovery: restore %s: %v", dotGit, err)
		return false
	}
	if err := os.Remove(bak); err != nil {
		log.Printf("cbm: git swap recovery: remove backup: %v", err)
	}
	log.Printf("cbm: git swap recovery: restored %s", dotGit)
	return true
}

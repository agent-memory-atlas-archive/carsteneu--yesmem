package models

import "testing"

// YM-115: two worktree layouts exist in this codebase. CanonicalProject only
// stripped "/.worktrees/", so a session inside "/.claude/worktrees/<name>" kept
// the worktree directory name as its canonical project, while the stored
// learnings of that path already carry the repository name.
func TestLos25_CanonicalProjectCoversBothWorktreeLayouts(t *testing.T) {
	cases := map[string]string{
		"/home/tester/memory/yesmem":                                              "yesmem",
		"/home/tester/memory/yesmem/.worktrees/yesloop-los-25":                    "yesmem",
		"/home/tester/memory/yesmem/.claude/worktrees/feat+capability-memory":     "yesmem",
		"/home/tester/memory/yesmem/.claude/worktrees/feat+capability-memory/sub": "yesmem",
		"/var/www/html/ccm19/cookie-consent-management":                          "cookie-consent-management",
		"yesmem": "yesmem",
	}
	for input, want := range cases {
		if got := CanonicalProject(input); got != want {
			t.Errorf("CanonicalProject(%q) = %q, want %q", input, got, want)
		}
	}
}

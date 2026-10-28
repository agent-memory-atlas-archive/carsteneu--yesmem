package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-373 (OPS-93, key 1): capProjectVisible decided visibility from exact
// path-or-basename equality, while execute_cap decides reachability with
// capProjectMatches (canonical basename plus repository root). A cap saved from a
// worktree session is bound to the worktree path, so it was runnable through
// execute_cap but invisible in the catalog: the proxy never injected it, and the
// model did not know the cap existed.
//
// The rule is one question, so it has to answer the same on both paths.
func TestLos56gWorktreeBoundCapIsVisibleInItsWorktree(t *testing.T) {
	h, _ := mustHandler(t)
	main := t.TempDir()
	wt := makeWorktreeDir(t, main, "los56g")
	seedCapAutoActive(t, h, "los56g_worktree_bound", wt)

	caps := unmarshalCaps(t, h.handleGetActiveCaps(map[string]any{"thread_id": "t", "project": wt}))

	if !hasCap(caps, "los56g_worktree_bound") {
		t.Errorf("a cap bound to the caller's own worktree was missing from the catalog, got %v", capNames(caps))
	}
}

// The same cap is also visible from the main checkout: the worktree is the same
// project, and execute_cap already treats it that way.
func TestLos56gWorktreeBoundCapIsVisibleFromTheMainCheckout(t *testing.T) {
	h, _ := mustHandler(t)
	main := t.TempDir()
	wt := makeWorktreeDir(t, main, "los56g")
	seedCapAutoActive(t, h, "los56g_worktree_bound", wt)

	caps := unmarshalCaps(t, h.handleGetActiveCaps(map[string]any{"thread_id": "t", "project": main}))

	if !hasCap(caps, "los56g_worktree_bound") {
		t.Errorf("a cap bound to a worktree of this repository was missing from the main checkout's catalog, got %v", capNames(caps))
	}
}

// Widening the rule must not leak across repositories that share a basename. Both
// repositories therefore carry the SAME basename: only the repository root tells
// them apart, so deleting that half of the rule makes this test fail.
func TestLos56gWorktreeBoundCapStaysForeignInAnotherRepository(t *testing.T) {
	h, _ := mustHandler(t)
	ownMain := filepath.Join(t.TempDir(), "a", "yesmem")
	otherMain := filepath.Join(t.TempDir(), "b", "yesmem")
	for _, root := range []string{ownMain, otherMain} {
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir repo: %v", err)
		}
	}
	ownWT := makeWorktreeDir(t, ownMain, "los56g")
	otherWT := makeWorktreeDir(t, otherMain, "los56g")
	seedCapAutoActive(t, h, "los56g_worktree_bound", ownWT)

	caps := unmarshalCaps(t, h.handleGetActiveCaps(map[string]any{"thread_id": "t", "project": otherWT}))

	if hasCap(caps, "los56g_worktree_bound") {
		t.Errorf("a cap of another repository with the same basename appeared in this catalog, got %v", capNames(caps))
	}
}

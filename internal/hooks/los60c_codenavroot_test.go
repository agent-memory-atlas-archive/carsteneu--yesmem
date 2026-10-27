package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/codescan"
)

// YM-436 finding 3: the hook carried its own copy of the git ascent next to the one
// the index uses, and two implementations of one rule drift. The same afternoon
// produced findings about a basename against a path and about worktree folding, so
// the hook now asks the index instead of walking up itself.
//
// The test is structural on purpose: the behaviour table lives in
// TestLos59eCodeNavRoot, and what this one holds is that both sides really are the
// same answer. While the delegation stands it cannot fail; it fails the moment
// someone reintroduces the second ascent, which is the drift it guards. The checkout
// root is not models.RepositoryRoot, which folds the worktrees of one repository onto
// the main root: that folding belongs to the index key, while the hook anchors its
// lookup at the checkout the command runs in.
func TestLos60cCodeNavRootIsTheIndexAscent(t *testing.T) {
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	worktree := filepath.Join(root, "worktree")
	sub := filepath.Join(clone, "internal", "hooks")
	for _, dir := range []string{filepath.Join(clone, ".git"), sub, worktree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatalf("write gitdir file: %v", err)
	}

	for _, cwd := range []string{clone, sub, worktree, root} {
		if got, want := codeNavRoot(cwd), codescan.CheckoutRoot(cwd); got != want {
			t.Errorf("codeNavRoot(%q) = %q, the index ascent says %q", cwd, got, want)
		}
	}

	// A relative cwd names no checkout to ascend from, and the hook keeps it as
	// written instead of asking the ascent: the ascent would resolve it against this
	// process's working directory, not against the command's, so for such a value the
	// two answers legitimately differ.
	if got := codeNavRoot("internal/hooks"); got != "internal/hooks" {
		t.Errorf("codeNavRoot(%q) = %q, want it unchanged", "internal/hooks", got)
	}

	// The empty value is the one answer the hook keeps for itself: it names no path
	// at all, and its callers read "" as "no cwd". The ascent is defined for a
	// directory and answers the current one, which is not the same statement.
	if got := codeNavRoot(""); got != "" {
		t.Errorf("codeNavRoot(\"\") = %q, want \"\"", got)
	}
}

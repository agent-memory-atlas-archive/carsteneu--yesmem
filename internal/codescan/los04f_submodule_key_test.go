package codescan

import (
	"os"
	"path/filepath"
	"testing"
)

// los04fWriteSubmodule turns sub into a submodule of super whose git data sits
// below super/.git/modules, using the layout git writes: a .git file with a
// relative gitdir link.
func los04fWriteSubmodule(t *testing.T, super, sub string) {
	t.Helper()
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(sub, moduleGitDir(super, sub))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(moduleGitDir(super, sub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: "+rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// moduleGitDir mirrors git's module path: every submodule path component, so a
// submodule named "modules" produces a doubled segment.
func moduleGitDir(super, sub string) string {
	rel, err := filepath.Rel(super, sub)
	if err != nil {
		return filepath.Join(super, ".git", "modules")
	}
	return filepath.Join(super, ".git", "modules", rel)
}

// A submodule keeps its git data in the superproject's .git/modules/<name> and
// carries a .git file pointing there. Resolving that to the superproject makes
// both repositories share one project_scan row, so whichever scans last
// overwrites the other's file list (YM-311, escaped from YM-277).
func TestScanCacheKey_SubmoduleKeepsItsOwnRepository(t *testing.T) {
	root := t.TempDir()
	super := los04eRepoAt(t, root, "super", "head-super")

	nested := filepath.Join(super, "libs", "inner")
	los04fWriteSubmodule(t, super, nested)

	if got := ScanCacheKey(super); got != super {
		t.Fatalf("ScanCacheKey(superproject) = %q, want %q", got, super)
	}
	if got := ScanCacheKey(nested); got != nested {
		t.Fatalf("ScanCacheKey(submodule) = %q, want the submodule root %q", got, nested)
	}
	if got := ScanCacheKey(filepath.Join(nested, "pkg")); got != nested {
		t.Fatalf("ScanCacheKey below the submodule = %q, want %q", got, nested)
	}
	if ScanCacheKey(nested) == ScanCacheKey(super) {
		t.Fatal("superproject and submodule share one scan cache row")
	}
}

// A submodule that is checked out in a linked worktree of the superproject
// carries both scopes in its gitdir (<super>/.git/worktrees/<wt>/modules/<sub>).
// It must not fall back onto the superproject, or both share one row again.
func TestScanCacheKey_SubmoduleInLinkedWorktreeOfSuperproject(t *testing.T) {
	root := t.TempDir()
	super := los04eRepoAt(t, root, "super", "head-super")

	wtGitDir := filepath.Join(super, ".git", "worktrees", "wt")
	moduleGitDir := filepath.Join(wtGitDir, "modules", "libs", "lib")
	if err := os.MkdirAll(moduleGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "super-wt")
	sub := filepath.Join(wt, "libs", "lib")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(sub, moduleGitDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: "+rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	key := ScanCacheKey(sub)
	if key == ScanCacheKey(super) {
		t.Fatalf("submodule in the superproject's worktree shares the superproject's key %q", key)
	}
	if key != sub {
		t.Fatalf("ScanCacheKey = %q, want the submodule checkout %q", key, sub)
	}
}

// A linked worktree may itself be called "modules". Its name is a name, not a
// submodule scope: the worktree keeps its own checkout as its key (YM-453), so
// no name can fold it onto another repository.
func TestScanCacheKey_WorktreeNamedModules(t *testing.T) {
	root := t.TempDir()
	super := los04eRepoAt(t, root, "super", "head-super")

	wtGitDir := filepath.Join(super, ".git", "worktrees", "modules")
	if err := os.MkdirAll(wtGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "wt-modules")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wt, wtGitDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := ScanCacheKey(wt); got != wt {
		t.Fatalf("ScanCacheKey(worktree named modules) = %q, want the worktree root %q", got, wt)
	}
}

// git reuses the segment name for real path components, so a submodule can be
// called "modules" itself. The reconstruction must not swallow such a component:
// otherwise the key collapses back onto the superproject (or onto a directory
// that is no repository at all).
func TestScanCacheKey_SubmoduleNamedModules(t *testing.T) {
	root := t.TempDir()
	super := los04eRepoAt(t, root, "super", "head-super")

	cases := []string{
		filepath.Join(super, "modules"),
		filepath.Join(super, "libs", "modules"),
		filepath.Join(super, "modules", "foo"),
	}
	for _, sub := range cases {
		t.Run(filepath.Base(sub), func(t *testing.T) {
			// Each case starts from a clean superproject layout: the module
			// directory of an earlier case would otherwise stay behind.
			if err := os.RemoveAll(filepath.Join(super, ".git", "modules")); err != nil {
				t.Fatal(err)
			}
			los04fWriteSubmodule(t, super, sub)

			if got := ScanCacheKey(sub); got != sub {
				t.Fatalf("ScanCacheKey(%q) = %q, want the submodule root", sub, got)
			}
			if got := ScanCacheKey(super); got != super {
				t.Fatalf("ScanCacheKey(superproject) = %q, want %q", got, super)
			}
		})
	}
}

package codescan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-232 (security review): a link whose target does not exist yet cannot be
// resolved, so the containment check accepted it and a later read followed it
// once the target appeared.
func TestResolveWithinRoot_RejectsDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "later.go"), filepath.Join(root, "dangle.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if target, ok := ResolveWithinRoot(root, "dangle.go"); ok {
		t.Errorf("a dangling symlink was accepted as inside the root: %q", target)
	}
	// Same rule when the unresolvable link sits in the middle of the path, or
	// when the chain ends in nothing.
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "gone"), filepath.Join(root, "dirlink")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, ok := ResolveWithinRoot(root, filepath.Join("dirlink", "child.go")); ok {
		t.Errorf("a path below an unresolvable directory link was accepted")
	}
	if err := os.Symlink(filepath.Join(root, "dangle.go"), filepath.Join(root, "chain.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, ok := ResolveWithinRoot(root, "chain.go"); ok {
		t.Errorf("a link chain ending in nothing was accepted")
	}

	// A plain path that does not exist yet stays legitimate: no link to follow.
	if _, ok := ResolveWithinRoot(root, "new.go"); !ok {
		t.Errorf("a plain path that does not exist yet must stay accepted")
	}
	// A new file below a directory link that stays inside the root is fine too.
	inside := filepath.Join(root, "realdir")
	if err := os.MkdirAll(inside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(root, "indir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, ok := ResolveWithinRoot(root, filepath.Join("indir", "new.go")); !ok {
		t.Errorf("a new file below an in-root directory link must stay accepted")
	}
}

// YM-232 (consequence check): ProjectKey and the cooldown key are derived from
// the root string, so canonicalizing only at the daemon caller split the key
// space in two for a project reached once by link and once by real path.
func TestProjectKey_CollapsesSymlinkedRoot(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "realrepo")
	if err := os.MkdirAll(filepath.Join(real, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "linkedrepo")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Positive control: the key names the repository at all.
	if got := ProjectKey(real); got != "realrepo" {
		t.Fatalf("ProjectKey(%q) = %q, want realrepo", real, got)
	}
	if got, want := ProjectKey(link), ProjectKey(real); got != want {
		t.Errorf("ProjectKey split by a symlinked root: %q vs %q", got, want)
	}
	if got, want := scanCooldownKey(link), scanCooldownKey(real); got != want {
		t.Errorf("scanCooldownKey split by a symlinked root: %q vs %q", got, want)
	}
	if got := cbmProjectName(link); got != cbmProjectName(real) {
		t.Errorf("CBM project name split by a symlinked root: %q vs %q", got, cbmProjectName(real))
	}

	// A tail that does not exist yet must not reintroduce the split: the
	// longest existing prefix is what gets resolved.
	missing := filepath.Join("sub", "newdir")
	if got, want := ProjectKey(filepath.Join(link, missing)), ProjectKey(filepath.Join(real, missing)); got != want {
		t.Errorf("a non-existent tail split the key again: %q vs %q", got, want)
	}
}

// ResolveScanRoot is applied at every layer (caller, cache, keys), so it has to
// be idempotent for every root shape.
func TestResolveScanRoot_IsIdempotent(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "proj")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(project, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Chdir(parent)

	for _, root := range []string{".", "proj", "proj/.", "proj/..", link, project, filepath.Join(link, "b")} {
		once := ResolveScanRoot(root)
		if twice := ResolveScanRoot(once); twice != once {
			t.Errorf("ResolveScanRoot not idempotent for %q: %q then %q", root, once, twice)
		}
	}
	// Value oracle: equality with itself is not enough, the dot form has to
	// land on the absolute project path.
	if got := ResolveScanRoot("proj/."); got != project {
		t.Errorf(`ResolveScanRoot("proj/.") = %q, want %q`, got, project)
	}
}

// YM-232 (cold review): the graph cache is read through the same root string it
// is written with, so the reader has to canonicalize like the writer.
func TestGetCachedGraph_CollapsesSymlinkedRoot(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "proj")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// The scan cache needs a git HEAD to have anything to key on.
	if err := os.MkdirAll(filepath.Join(real, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, ".git", "HEAD"), []byte("0123456789abcdef0123456789abcdef01234567\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	scanner := NewCachedScanner(&DirectoryScanner{})
	if _, err := scanner.Scan(link); err != nil {
		t.Fatalf("unexpected scan error: %v", err)
	}
	if scanner.GetCachedGraph(link) == nil {
		t.Errorf("the graph was not cached under the link root")
	}
	if scanner.GetCachedGraph(real) == nil {
		t.Errorf("the real path did not reach the graph cached via the link")
	}
}

// YM-232 (cold review, Important): the scan error text carried the
// client-supplied directory unquoted, so a forged log line survived the %q on
// the daemon's log line.
func TestCBMScanner_QuotesPathInErrors(t *testing.T) {
	blacklisted := filepath.Join(t.TempDir(), "proj\nFORGED LINE")
	if _, err := (&CBMScanner{bin: "/bin/true"}).Scan(blacklisted); err == nil {
		t.Fatal("expected an error for a directory without a repository")
	} else if strings.Contains(err.Error(), "proj\nFORGED") {
		t.Errorf("blacklist error carries an unquoted path: %q", err.Error())
	}

	repo := filepath.Join(t.TempDir(), "repo\nFORGED LINE")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "cbm")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 3\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := (&CBMScanner{bin: script}).Scan(repo); err == nil {
		t.Fatal("expected the query to fail")
	} else if strings.Contains(err.Error(), "repo\nFORGED") {
		t.Errorf("query error carries an unquoted path: %q", err.Error())
	}
}

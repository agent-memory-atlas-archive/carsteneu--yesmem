package codescan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-232 / DEV-DT42: the regex scanner (the fallback when no CBM binary is
// installed) reads every source file the walk reports. filepath.Walk reports a
// symlink as its own file entry, so a link pointing out of the root hands the
// scanner a foreign file's content and signatures.
func TestDirectoryScanner_DoesNotReadSymlinkOutOfRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(target, []byte("package secret\n\nfunc LeakedOutOfRoot() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.go"), []byte("package inside\n\nfunc KeptInside() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The positive control: containment is the rule, not a blanket skip of links.
	if err := os.Symlink(filepath.Join(root, "inside.go"), filepath.Join(root, "alias.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result, err := (&DirectoryScanner{}).Scan(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range result.Files {
		for _, sig := range f.Signatures {
			if strings.Contains(sig, "LeakedOutOfRoot") {
				t.Errorf("scanner indexed symbols from a file outside the root (via %s)", f.Path)
			}
		}
		if f.Path == "link.go" {
			t.Errorf("scanner kept a symlink entry whose target lies outside the root")
		}
		// The inside link must be indexed through its resolved target, with
		// content, not as an empty shell.
		if f.Path == "alias.go" && (f.Content == "" || len(f.Signatures) == 0) {
			t.Errorf("an in-root symlink was indexed without content: %+v", f)
		}
	}
	if len(result.Files) != 2 {
		t.Errorf("expected inside.go and alias.go, got %d files: %+v", len(result.Files), result.Files)
	}
}

// YM-232: filepath.Walk lstats its root, so a root that is itself a symlink is
// reported as a plain file entry and the tree below it is never entered.
func TestDirectoryScanner_ScansSymlinkedRoot(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result, err := (&DirectoryScanner{}).Scan(link)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "a.go" {
		t.Fatalf("a symlinked root must be scanned, got %d files: %+v", len(result.Files), result.Files)
	}
}

// YM-232: shouldSkipDir judges the ROOT entry by info.Name(); for a root given
// as "." (or "proj/.") that name is ".", so the walk was pruned at the root and
// the scan came back empty.
func TestDirectoryScanner_ScansRootGivenAsDot(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "proj")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)

	for _, root := range []string{".", "proj", "proj/.", "proj/.."} {
		result, err := (&DirectoryScanner{}).Scan(root)
		if err != nil {
			t.Fatalf("root %q: unexpected error: %v", root, err)
		}
		if len(result.Files) != 1 {
			t.Errorf("root %q returned %d files, want 1: %+v", root, len(result.Files), result.Files)
		}
	}
}

// YM-232: DetectTier is the same walk without the file read; it counted files
// reached through a symlink and ignored a root that is a link or a dot form.
func TestDetectTier_SymlinkAndRootForms(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(target, []byte("package secret\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, stats := DetectTier(link); stats.FileCount != 1 {
		t.Errorf("symlinked root: FileCount = %d, want 1", stats.FileCount)
	}

	if err := os.Symlink(target, filepath.Join(real, "leak.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, stats := DetectTier(real); stats.FileCount != 1 {
		t.Errorf("out-of-root symlink was counted: FileCount = %d, want 1", stats.FileCount)
	}

	t.Chdir(parent)
	if _, stats := DetectTier("."); stats.FileCount != 1 {
		t.Errorf(`root "." counted %d files, want 1`, stats.FileCount)
	}
}

// YM-232: ScanHealth reads every reported source file (TODO counting), so it
// inherits both the symlink read and the root-form pruning.
func TestScanHealth_DoesNotReadSymlinkOutOfRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(target, []byte("package secret\n\n// TODO: outside\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.go"), []byte("package inside\n\n// TODO: inside\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Exactly one: the TODO inside the root. A broken counter that always
	// returns 0 would pass a "not 1" assertion, so the value is pinned.
	if health := ScanHealth(root); health.TodoCount != 1 {
		t.Errorf("health scan counted %d TODOs, want 1 (only the one inside the root)", health.TodoCount)
	}
}

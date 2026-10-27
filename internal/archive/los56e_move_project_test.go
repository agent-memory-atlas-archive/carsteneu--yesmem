package archive

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestLos56eMoveProjectDirKeepsTheMoreCompleteCopy pins the collision rules of
// the merge a project rename depends on (YM-320, review round): a name that
// exists on both sides is settled by content — the shorter file only goes when it
// is a byte prefix of the longer one — and an entry that is not a plain file is
// left alone instead of moved or deleted.
func TestLos56eMoveProjectDirKeepsTheMoreCompleteCopy(t *testing.T) {
	base := t.TempDir()
	a := New(base)

	fromDir := filepath.Join(base, "from-project")
	toDir := filepath.Join(base, "to-project")
	if err := os.MkdirAll(fromDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(toDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The source is the grown copy of the destination (destination is its prefix).
	if err := os.WriteFile(filepath.Join(fromDir, "ses-1.jsonld"), []byte("complete-document"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toDir, "ses-1.jsonld"), []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The source is contained in the destination (source is its prefix).
	if err := os.WriteFile(filepath.Join(fromDir, "ses-2.jsonld"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toDir, "ses-2.jsonld"), []byte("older-and-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same name, neither contains the other: two different documents.
	if err := os.WriteFile(filepath.Join(fromDir, "ses-3.jsonld"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toDir, "ses-3.jsonld"), []byte("beta-beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not a plain file: nothing to move, nothing to delete.
	if err := os.MkdirAll(filepath.Join(fromDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}

	moved, kept, err := a.MoveProjectDir("from-project", "to-project")
	if err != nil {
		t.Fatalf("move project dir: %v", err)
	}
	if moved != 1 || kept != 3 {
		t.Fatalf("moved=%d kept=%d, want 1 moved (ses-1) and 3 kept", moved, kept)
	}
	if body, err := os.ReadFile(filepath.Join(toDir, "ses-1.jsonld")); err != nil || string(body) != "complete-document" {
		t.Fatalf("the grown source must replace its truncated copy: %v %q", err, string(body))
	}
	if body, err := os.ReadFile(filepath.Join(toDir, "ses-2.jsonld")); err != nil || string(body) != "older-and-longer" {
		t.Fatalf("the containing destination copy must survive: %v %q", err, string(body))
	}
	if _, err := os.Stat(filepath.Join(fromDir, "ses-2.jsonld")); err == nil {
		t.Error("a contained source copy must not stay behind")
	}
	if body, err := os.ReadFile(filepath.Join(fromDir, "ses-3.jsonld")); err != nil || string(body) != "alpha" {
		t.Fatalf("two different documents must both survive: %v %q", err, string(body))
	}
	if body, err := os.ReadFile(filepath.Join(toDir, "ses-3.jsonld")); err != nil || string(body) != "beta-beta" {
		t.Fatalf("two different documents must both survive: %v %q", err, string(body))
	}
	if _, err := os.Stat(filepath.Join(fromDir, "ses-1.jsonld")); err == nil {
		t.Error("the moved document must not stay behind")
	}
	if _, err := os.Stat(filepath.Join(fromDir, "nested")); err != nil {
		t.Error("a nested directory must be left where it is")
	}
	if _, _, err := a.MoveProjectDir("does-not-exist", "to-project"); err != nil {
		t.Fatalf("a missing source folder is not an error: %v", err)
	}
	if _, _, err := a.MoveProjectDir("../escape", "to-project"); err == nil {
		t.Error("a project name that is not a plain directory name must be refused")
	}
}

// TestLos56eMoveProjectDirComparesTheWholeFile is the round-2 review finding on
// YM-320: the collision rule used the sampled prefixMatches, which only looks at
// the start, the middle and the end of the shorter file. Two 2000-byte documents
// that differ only inside a gap between those windows looked identical, and the
// source was deleted. A branch that deletes must compare the whole range.
func TestLos56eMoveProjectDirComparesTheWholeFile(t *testing.T) {
	base := t.TempDir()
	a := New(base)

	fromDir := filepath.Join(base, "from-project")
	toDir := filepath.Join(base, "to-project")
	for _, dir := range []string{fromDir, toDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	makeDoc := func(marker byte) []byte {
		buf := bytes.Repeat([]byte("a"), 2000)
		copy(buf[0:], []byte("same-start-"))
		// A region no sampled window covers (windows are 0-255, 872-1127, 1744-1999).
		copy(buf[350:], bytes.Repeat([]byte{marker}, 10))
		copy(buf[1900:], []byte("same-tail"))
		return buf
	}
	if err := os.WriteFile(filepath.Join(fromDir, "ses-sampled.jsonld"), makeDoc('x'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toDir, "ses-sampled.jsonld"), makeDoc('y'), 0o600); err != nil {
		t.Fatal(err)
	}

	moved, kept, err := a.MoveProjectDir("from-project", "to-project")
	if err != nil {
		t.Fatalf("move project dir: %v", err)
	}
	if moved != 0 || kept != 1 {
		t.Fatalf("moved=%d kept=%d, want 0 moved and 1 kept (two different documents)", moved, kept)
	}
	if _, err := os.Stat(filepath.Join(fromDir, "ses-sampled.jsonld")); err != nil {
		t.Fatal("a document that only agrees inside the sampled windows must not be deleted")
	}
	if body, err := os.ReadFile(filepath.Join(toDir, "ses-sampled.jsonld")); err != nil || body[350] != 'y' {
		t.Fatalf("the destination copy must stay untouched: %v", err)
	}

	// A real duplicate is still recognised and removed.
	dup := bytes.Repeat([]byte("d"), 2000)
	if err := os.WriteFile(filepath.Join(fromDir, "ses-dup.jsonld"), dup, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toDir, "ses-dup.jsonld"), dup, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, kept, err = a.MoveProjectDir("from-project", "to-project"); err != nil {
		t.Fatalf("second move: %v", err)
	}
	// The divergent document is still kept, the identical duplicate is gone.
	if kept != 2 {
		t.Fatalf("kept=%d, want 2 (divergent document + removed duplicate)", kept)
	}
	if _, err := os.Stat(filepath.Join(fromDir, "ses-dup.jsonld")); err == nil {
		t.Fatal("a byte-identical duplicate must be removed from the source side")
	}
}

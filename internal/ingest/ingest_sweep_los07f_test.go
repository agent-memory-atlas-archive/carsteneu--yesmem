package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-364 [DEV-DT57]: a multi-path run never swept. The sweep was anchored on
// basePath, "the first path that yielded files", which moves between runs, and
// the guard required the registered path to equal it. The registered path is
// what the stored rows are keyed on, so that is the tree the sweep has to
// compare against.
//
// A moved base also re-keys the documents of the other roots without removing
// the old rows: the second run leaves "other.md" and "../second/other.md" for
// the same document. That duplication predates this fix and is accepted here;
// the assertions below only pin what the sweep decides.
func TestDocIngest_MultiPathRunSweepsDeletedDocumentLos07f(t *testing.T) {
	store := mustOpenStore(t)

	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "keep.md"), []byte("# Keep\n\nProse that stays."), 0644); err != nil {
		t.Fatalf("write keep: %v", err)
	}
	gone := filepath.Join(first, "gone.md")
	if err := os.WriteFile(gone, []byte("# Gone\n\nProse that will be deleted."), 0644); err != nil {
		t.Fatalf("write gone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second, "other.md"), []byte("# Other\n\nProse in the second root."), 0644); err != nil {
		t.Fatalf("write other: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "multi-sweep"}
	if _, err := Run(cfg, []string{first, second}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("multi-sweep", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if src.Path != first {
		t.Fatalf("registered path = %q, want the first root %q", src.Path, first)
	}
	if chunks, _ := store.GetDocChunksByFile("gone.md", src.ID); len(chunks) == 0 {
		t.Fatal("first ingest did not index gone.md")
	}

	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove gone.md: %v", err)
	}

	// The other root comes first this time, so basePath moves to it while the
	// registered path stays the first root.
	result, err := Run(cfg, []string{second, first}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile("gone.md", src.ID); len(chunks) != 0 {
		t.Errorf("%d chunk(s) of the deleted document gone.md survived the multi-path re-ingest", len(chunks))
	}
	if chunks, _ := store.GetDocChunksByFile("keep.md", src.ID); len(chunks) == 0 {
		t.Error("the sweep took the chunks of a document that is still there")
	}
	if result.FilesRemoved != 1 {
		t.Errorf("files removed = %d, want 1 (the deleted document)", result.FilesRemoved)
	}
}

// YM-364, cold-review finding: anchoring the sweep on the registered path made a
// live document a deletion candidate when the anchor moved and the content
// matched an older row. processFile keeps such a row instead of writing a new
// one, so the only witness for the document is a key in the old anchor's
// namespace. The run must count the keys it maps under both anchors.
func TestDocIngest_MovedAnchorKeepsADocumentThatMatchedAnOldRowLos07f(t *testing.T) {
	store := mustOpenStore(t)

	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	content := "# Doc\n\nIdentical prose in both roots."
	if err := os.WriteFile(filepath.Join(first, "doc.md"), []byte(content), 0644); err != nil {
		t.Fatalf("write first doc: %v", err)
	}
	// This document is deleted later, so the test can prove the sweep ran
	// instead of merely asserting that something survived.
	if err := os.WriteFile(filepath.Join(first, "keep.md"), []byte("# Keep\n\nProse that will be deleted."), 0644); err != nil {
		t.Fatalf("write first keep: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "moved-anchor"}
	if _, err := Run(cfg, []string{first}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("moved-anchor", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := store.GetDocChunksByFile("doc.md", src.ID); len(chunks) == 0 {
		t.Fatal("first ingest did not index doc.md")
	}

	// The first root loses both documents, so the next run's basePath moves to
	// the second root. The second root holds a document with the same content:
	// processFile matches the old row by hash and writes nothing.
	for _, name := range []string{"doc.md", "keep.md"} {
		if err := os.Remove(filepath.Join(first, name)); err != nil {
			t.Fatalf("remove first/%s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(second, "doc.md"), []byte(content), 0644); err != nil {
		t.Fatalf("write second doc: %v", err)
	}

	result, err := Run(cfg, []string{first, second}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile("doc.md", src.ID); len(chunks) == 0 {
		t.Error("the sweep removed the chunks of second/doc.md, which is still on disk, because its content matched the old row")
	}
	if result.FilesRemoved != 1 {
		t.Fatalf("files removed = %d, want 1 (first/keep.md): the sweep did not run, so the survival above proves nothing", result.FilesRemoved)
	}
	if chunks, _ := store.GetDocChunksByFile("keep.md", src.ID); len(chunks) != 0 {
		t.Error("the sweep kept the chunks of first/keep.md, which is gone from disk")
	}
}

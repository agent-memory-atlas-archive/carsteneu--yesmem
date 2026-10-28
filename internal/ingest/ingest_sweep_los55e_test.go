package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-374 [DEV-DT59], part 2: a document of a second root is keyed "../<root>"
// relative to the registered base, and the sweep skipped every key outside that
// base. A deleted document of the second root therefore stayed in the index
// forever, even though the run walked that root completely and could see it was
// gone.
func TestDocIngest_SweepsDeletedDocumentOfAnotherRootLos55e(t *testing.T) {
	store := mustOpenStore(t)

	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "keep.md"), []byte("# Keep\n\nProse of the first root."), 0644); err != nil {
		t.Fatalf("write first keep: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second, "keep2.md"), []byte("# Keep2\n\nProse of the second root."), 0644); err != nil {
		t.Fatalf("write second keep: %v", err)
	}
	gone := filepath.Join(second, "gone.md")
	if err := os.WriteFile(gone, []byte("# Gone\n\nProse that will be deleted."), 0644); err != nil {
		t.Fatalf("write second gone: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "second-root-sweep"}
	if _, err := Run(cfg, []string{first, second}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("second-root-sweep", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := store.GetDocChunksByFile("../second/gone.md", src.ID); len(chunks) == 0 {
		t.Fatal("first ingest did not key the second root's document as ../second/gone.md")
	}

	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove gone.md: %v", err)
	}

	result, err := Run(cfg, []string{first, second}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile("../second/gone.md", src.ID); len(chunks) != 0 {
		t.Errorf("%d chunk(s) of the deleted document in the second root survived the sweep", len(chunks))
	}
	if chunks, _ := store.GetDocChunksByFile("../second/keep2.md", src.ID); len(chunks) == 0 {
		t.Error("the sweep took the chunks of a document that is still there")
	}
	if result.FilesRemoved != 1 {
		t.Errorf("files removed = %d, want 1 (the deleted document of the second root)", result.FilesRemoved)
	}
}

// The counter-case: a "../" key whose root this run did NOT walk is not this
// run's business. The daemon syncs one path at a time, so a multi-path
// registration is exactly the shape that has such rows without having walked
// them.
func TestDocIngest_KeepsForeignRootRowsOfAnUnwalkedRootLos55e(t *testing.T) {
	store := mustOpenStore(t)

	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "keep.md"), []byte("# Keep\n\nProse of the first root."), 0644); err != nil {
		t.Fatalf("write first keep: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second, "other.md"), []byte("# Other\n\nProse of the second root."), 0644); err != nil {
		t.Fatalf("write second other: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "single-path-run"}
	if _, err := Run(cfg, []string{first, second}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("single-path-run", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}

	// Only the registered root is synced, as the daemon does it. The second
	// root was not walked, so this pass cannot say anything about its documents.
	result, err := Run(cfg, []string{first}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile("../second/other.md", src.ID); len(chunks) == 0 {
		t.Error("the sweep removed the chunks of a root this run never walked")
	}
	if result.FilesRemoved != 0 {
		t.Errorf("files removed = %d, want 0 (nothing this run could observe went away)", result.FilesRemoved)
	}
}

// The security property of the foreign-root rule: a root walked only partially
// says nothing about the documents below the part that failed. Their keys must
// stay, or an unreadable subdirectory would delete the index of everything under
// it.
func TestDocIngest_IncompleteForeignRootWalkKeepsItsRowsLos55e(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	store := mustOpenStore(t)

	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	blocked := filepath.Join(second, "blocked")
	for _, dir := range []string{first, second, blocked} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "keep.md"), []byte("# Keep\n\nProse of the first root."), 0644); err != nil {
		t.Fatalf("write first keep: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "hidden.md"), []byte("# Hidden\n\nProse below the unreadable directory."), 0644); err != nil {
		t.Fatalf("write hidden: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "incomplete-foreign"}
	if _, err := Run(cfg, []string{first, second}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("incomplete-foreign", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := store.GetDocChunksByFile("../second/blocked/hidden.md", src.ID); len(chunks) == 0 {
		t.Fatal("first ingest did not key the document below second/blocked")
	}

	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatalf("chmod blocked: %v", err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0755) })

	result, err := Run(cfg, []string{first, second}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if result.UnreadableEntries == 0 {
		t.Fatal("the unreadable subtree was not reported, so this test proves nothing")
	}
	if chunks, _ := store.GetDocChunksByFile("../second/blocked/hidden.md", src.ID); len(chunks) == 0 {
		t.Error("the sweep deleted the chunks of a foreign root it walked only partially")
	}
	if result.FilesRemoved != 0 {
		t.Errorf("files removed = %d, want 0: no root was walked completely under a failed subtree", result.FilesRemoved)
	}
}

// YM-374 [DEV-DT59], part 3: when the registered root yields nothing, the sweep
// judged every key inside it as deleted. After the base moved, the plain keys of
// the earlier anchor are indistinguishable from the registered anchor's, so a
// document that is still on disk lost its chunks and its learnings.
func TestDocIngest_EmptyRegisteredRootKeepsDocumentsOfAnEarlierAnchorLos55e(t *testing.T) {
	store := mustOpenStore(t)

	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "a.md"), []byte("# A\n\nProse of the first root."), 0644); err != nil {
		t.Fatalf("write first a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second, "x.md"), []byte("# X\n\nProse of the second root."), 0644); err != nil {
		t.Fatalf("write second x: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "moved-base-empty"}
	if _, err := Run(cfg, []string{first, second}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	// The other root comes first now, so the stored base path moves to it. The
	// first root's document keeps its plain key from the earlier run.
	if _, err := Run(cfg, []string{second, first}, store); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	src, err := store.GetDocSource("moved-base-empty", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if src.Path != second {
		t.Fatalf("registered path = %q, want the moved base %q", src.Path, second)
	}
	if chunks, _ := store.GetDocChunksByFile("a.md", src.ID); len(chunks) == 0 {
		t.Fatal("the second ingest already lost a.md, which is the setup for this case")
	}

	// The registered root empties, the first root still holds its document. The
	// run observes nothing, so it must not decide about a.md.
	if err := os.Remove(filepath.Join(second, "x.md")); err != nil {
		t.Fatalf("remove x.md: %v", err)
	}
	if _, err := Run(cfg, []string{second}, store); err != nil {
		t.Fatalf("third ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile("a.md", src.ID); len(chunks) == 0 {
		t.Error("an empty run of the registered root deleted a.md, whose file is still on disk under the earlier anchor")
	}
	// The sweep still has to work where it can attribute: the ../ key of the
	// root this run actually walked is a deletion.
	if chunks, _ := store.GetDocChunksByFile("../second/x.md", src.ID); len(chunks) != 0 {
		t.Errorf("%d chunk(s) of the deleted document in the walked root survived", len(chunks))
	}
}

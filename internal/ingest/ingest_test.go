package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

func mustOpenStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDocIngest_CreatesChunks(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	docFile := filepath.Join(dir, "docs.md")
	os.WriteFile(docFile, []byte("# Regular Docs\n\nJust documentation."), 0644)

	cfg := Config{
		Project: "test-proj",
		Name:    "regular-docs",
	}

	_, err := Run(cfg, []string{docFile}, store)
	if err != nil {
		t.Fatalf("ingest run: %v", err)
	}

	ds, err := store.GetDocSource("regular-docs", "test-proj")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}

	// Non-skill docs should NOT have full_content
	if ds.FullContent != "" {
		t.Errorf("full_content should be empty for non-skill docs, got %q", ds.FullContent)
	}
}

// YM-231: a source registered with a relative path stored that string verbatim.
// Every later reader resolves it against its own working directory, so doc-sync
// re-read a different tree under the same source name. The registration is the
// only point where the working directory is meaningful, so the path is resolved
// there and stored absolute.
func TestDocIngest_StoresAbsoluteSourcePath(t *testing.T) {
	store := mustOpenStore(t)

	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "notes"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "notes", "guide.md"), []byte("# Guide\n\nBody."), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Chdir(work)

	cfg := Config{Project: "test-proj", Name: "notes-docs"}
	if _, err := Run(cfg, []string{"notes"}, store); err != nil {
		t.Fatalf("ingest run: %v", err)
	}

	ds, err := store.GetDocSource("notes-docs", "test-proj")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}
	if !filepath.IsAbs(ds.Path) {
		t.Fatalf("stored path %q is relative — a later reader resolves it against its own cwd", ds.Path)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(work, "notes"))
	got, _ := filepath.EvalSymlinks(ds.Path)
	if got != want {
		t.Errorf("stored path = %q, want %q", ds.Path, filepath.Join(work, "notes"))
	}
}

// YM-231: a relative path was resolved against the working directory of the
// caller. An empty one is the extreme case: filepath.Abs("") is the cwd, so the
// whole current directory would be indexed as the source.
func TestDocIngest_RejectsEmptyPath(t *testing.T) {
	store := mustOpenStore(t)
	t.Chdir(t.TempDir())

	if _, err := Run(Config{Project: "test-proj", Name: "empty-path"}, []string{""}, store); err == nil {
		t.Fatal("ingest accepted an empty path, which filepath.Abs silently turns into the current directory")
	}
}

func TestDocIngest_RSTFile(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	rstContent := "Messenger Component\n====================\n\n" +
		"The Messenger component helps sending messages to a message bus.\n\n" +
		"Configuration\n-------------\n\n" +
		".. versionadded:: 6.1\n\n" +
		"    The messenger:consume command was added in Symfony 6.1.\n\n" +
		"Use :class:`Symfony\\Component\\Messenger\\MessageBus` to dispatch messages.\n\n" +
		".. code-block:: php\n\n" +
		"    $bus->dispatch(new MyMessage());\n\n" +
		".. warning::\n\n" +
		"    Always configure a failure transport.\n\n" +
		"Retry Strategy\n~~~~~~~~~~~~~~\n\n" +
		"See :doc:`/messenger/retry` and :func:`retry` for details.\n\n" +
		".. code-block:: yaml\n\n" +
		"    framework:\n        messenger:\n            failure_transport: failed\n"
	rstFile := filepath.Join(dir, "messenger.rst")
	os.WriteFile(rstFile, []byte(rstContent), 0644)

	cfg := Config{
		Project: "test-proj",
		Name:    "symfony-docs",
	}

	result, err := Run(cfg, []string{rstFile}, store)
	if err != nil {
		t.Fatalf("ingest run: %v", err)
	}

	if result.FilesProcessed != 1 {
		t.Errorf("expected 1 file processed, got %d", result.FilesProcessed)
	}
	if result.ChunksCreated == 0 {
		t.Fatal("expected chunks to be created")
	}

	// Verify chunks have correct structure
	ds, err := store.GetDocSource("symfony-docs", "test-proj")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}

	chunks, err := store.GetDocChunksBySource(ds.ID)
	if err != nil {
		t.Fatalf("get chunks: %v", err)
	}

	if len(chunks) == 0 {
		t.Fatal("expected chunks from RST file")
	}

	// Check heading paths exist
	paths := make(map[string]bool)
	for _, c := range chunks {
		paths[c.HeadingPath] = true
	}

	if !paths["Messenger Component > Configuration"] {
		t.Errorf("missing 'Messenger Component > Configuration' path, got: %v", paths)
	}

	// Check metadata on chunks
	var foundLanguages, foundVersionAdded, foundEntities, foundWarning bool
	for _, c := range chunks {
		meta := c.Metadata
		if meta["languages"] != "" {
			foundLanguages = true
		}
		if meta["version_added"] != "" {
			foundVersionAdded = true
		}
		if meta["rst_entities"] != "" {
			foundEntities = true
		}
		if meta["admonition"] != "" {
			foundWarning = true
		}
	}

	if !foundLanguages {
		t.Error("expected at least one chunk with languages metadata")
	}
	if !foundVersionAdded {
		t.Error("expected at least one chunk with version_added metadata")
	}
	if !foundEntities {
		t.Error("expected at least one chunk with rst_entities metadata")
	}
	if !foundWarning {
		t.Error("expected at least one chunk with admonition metadata")
	}
}

// YM-24: a single-file doc source always stores the relative path "." (the
// file is its own base path). Without a source_id filter the incremental sync
// therefore reads the chunks of EVERY single-file source and deletes the ones
// it does not recognise — a second ingest wipes the first source's chunks.
func TestDocIngest_TwoSingleFileSources_KeepChunks(t *testing.T) {
	store := mustOpenStore(t)

	dirA := t.TempDir()
	fileA := filepath.Join(dirA, "a.md")
	if err := os.WriteFile(fileA, []byte("# Alpha\n\nAlpha documentation body."), 0644); err != nil {
		t.Fatalf("write A: %v", err)
	}

	dirB := t.TempDir()
	fileB := filepath.Join(dirB, "b.md")
	if err := os.WriteFile(fileB, []byte("# Beta\n\nBeta documentation body."), 0644); err != nil {
		t.Fatalf("write B: %v", err)
	}

	if _, err := Run(Config{Project: "p", Name: "src-a"}, []string{fileA}, store); err != nil {
		t.Fatalf("ingest A: %v", err)
	}
	if _, err := Run(Config{Project: "p", Name: "src-b"}, []string{fileB}, store); err != nil {
		t.Fatalf("ingest B: %v", err)
	}

	srcA, err := store.GetDocSource("src-a", "p")
	if err != nil {
		t.Fatalf("get source A: %v", err)
	}
	chunksA, err := store.GetDocChunksBySource(srcA.ID)
	if err != nil {
		t.Fatalf("chunks of source A: %v", err)
	}
	if len(chunksA) == 0 {
		t.Fatalf("source A lost all chunks after ingesting source B (YM-24: chunk delete across sources)")
	}

	srcB, err := store.GetDocSource("src-b", "p")
	if err != nil {
		t.Fatalf("get source B: %v", err)
	}
	chunksB, err := store.GetDocChunksBySource(srcB.ID)
	if err != nil {
		t.Fatalf("chunks of source B: %v", err)
	}
	if len(chunksB) == 0 {
		t.Fatalf("source B has no chunks")
	}

	// The deleted chunks must be gone from FTS too — otherwise a stale index hit.
	hits, err := store.SearchDocChunksFTS("Alpha", "", "", "", "", 10, nil)
	if err != nil {
		t.Fatalf("fts search: %v", err)
	}
	if len(hits) == 0 {
		t.Errorf("source A is not reachable via full-text search anymore")
	}
}

// YM-24 regression guard: re-ingesting the SAME single-file source with changed
// content must replace its old chunks instead of accumulating or deleting them.
func TestDocIngest_SingleFileReingest_ReplacesOwnChunks(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(file, []byte("# Title\n\nFirst version body."), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Run(Config{Project: "p", Name: "src"}, []string{file}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if err := os.WriteFile(file, []byte("# Title\n\nSecond version body."), 0644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := Run(Config{Project: "p", Name: "src"}, []string{file}, store); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	src, err := store.GetDocSource("src", "p")
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	chunks, err := store.GetDocChunksBySource(src.ID)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatalf("source lost its chunks on re-ingest")
	}
	old, err := store.SearchDocChunksFTS("First", "", "", "", "", 10, nil)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(old) != 0 {
		t.Errorf("stale chunk of the previous revision still searchable: %d hits", len(old))
	}
}

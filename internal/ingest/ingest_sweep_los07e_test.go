package ingest

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// YM-329 [DEV-DT51]: a document deleted inside a walked tree never reaches
// processFile, because the walk simply does not yield it. Its chunks stayed in
// the index forever, so docs_search kept serving a file that no longer exists.
func TestDocIngest_RemovesChunksOfADocumentDeletedInASubdirectoryLos07e(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	gone := filepath.Join(sub, "gone.md")
	if err := os.WriteFile(gone, []byte("# Gone\n\nProse that will be deleted."), 0644); err != nil {
		t.Fatalf("write gone: %v", err)
	}
	kept := filepath.Join(dir, "kept.md")
	if err := os.WriteFile(kept, []byte("# Kept\n\nProse that stays."), 0644); err != nil {
		t.Fatalf("write kept: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "sweep-docs"}
	if _, err := Run(cfg, []string{dir}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("sweep-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := store.GetDocChunksByFile("sub/gone.md", src.ID); len(chunks) == 0 {
		t.Fatal("first ingest did not index sub/gone.md")
	}

	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove gone.md: %v", err)
	}

	result, err := Run(cfg, []string{dir}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile("sub/gone.md", src.ID); len(chunks) != 0 {
		t.Errorf("%d chunk(s) of the deleted document sub/gone.md survived the re-ingest", len(chunks))
	}
	if chunks, _ := store.GetDocChunksByFile("kept.md", src.ID); len(chunks) == 0 {
		t.Error("the sweep took the chunks of a document that is still there")
	}
	if result.FilesRemoved != 1 {
		t.Errorf("files removed = %d, want 1 (the deleted document)", result.FilesRemoved)
	}
}

// The sweep trusts the walked file set, so a walk that dropped an unreadable
// subtree must not sweep: every document below it would look deleted. This is
// the counterpart to the test above and is green on the unpatched tree by
// design, because there was no sweep to go wrong yet.
func TestDocIngest_IncompleteWalkDoesNotSweepAnUnreadableSubtreeLos07e(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	store := mustOpenStore(t)

	dir := t.TempDir()
	readable := filepath.Join(dir, "readable.md")
	if err := os.WriteFile(readable, []byte("# Readable\n\nProse."), 0644); err != nil {
		t.Fatalf("write readable: %v", err)
	}
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blocked, 0755); err != nil {
		t.Fatalf("mkdir blocked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "hidden.md"), []byte("# Hidden\n\nProse below a blocked directory."), 0644); err != nil {
		t.Fatalf("write hidden: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "guarded-docs"}
	if _, err := Run(cfg, []string{dir}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("guarded-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := store.GetDocChunksByFile("blocked/hidden.md", src.ID); len(chunks) == 0 {
		t.Fatal("first ingest did not index blocked/hidden.md")
	}

	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatalf("chmod blocked: %v", err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0755) })

	result, err := Run(cfg, []string{dir}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if result.UnreadableEntries == 0 {
		t.Fatal("the unreadable subtree was not reported, so this test proves nothing")
	}
	if chunks, _ := store.GetDocChunksByFile("blocked/hidden.md", src.ID); len(chunks) == 0 {
		t.Error("the sweep deleted the chunks of a document in an unreadable subtree")
	}
}

// A walk that dropped an entry is not a completed sync. Advancing last_sync
// anyway hides the unreadable subtree until some unrelated change happens, so
// the subtree is never retried.
func TestDocIngest_IncompleteWalkDoesNotAdvanceLastSyncLos07e(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	store := mustOpenStore(t)

	dir := t.TempDir()
	doc := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(doc, []byte("# Notes\n\nFirst version."), 0644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	cfg := Config{Project: "test-proj", Name: "stamp-docs"}
	if _, err := Run(cfg, []string{dir}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	const oldStamp = "2020-01-01 00:00:00"
	if _, err := store.DB().Exec(`UPDATE doc_sources SET last_sync = ?, chunk_count = 99 WHERE name = ?`, oldStamp, "stamp-docs"); err != nil {
		t.Fatalf("age last_sync: %v", err)
	}

	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blocked, 0755); err != nil {
		t.Fatalf("mkdir blocked: %v", err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatalf("chmod blocked: %v", err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0755) })
	if err := os.WriteFile(doc, []byte("# Notes\n\nSecond version."), 0644); err != nil {
		t.Fatalf("rewrite notes: %v", err)
	}

	result, err := Run(cfg, []string{dir}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if result.UnreadableEntries == 0 {
		t.Fatal("the unreadable subtree was not reported, so this test proves nothing")
	}

	src, err := store.GetDocSource("stamp-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if !src.LastSync.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last_sync advanced to %v on an incomplete walk, want the aged stamp; the unreadable subtree would never be retried", src.LastSync)
	}
	// The other half: the pass did index what it could see, so the chunk count
	// has to be refreshed even though the stamp stays put.
	if src.ChunkCount != 1 {
		t.Errorf("chunk_count = %d, want 1 refreshed from the chunks that were indexed", src.ChunkCount)
	}
}

// A doc source that is a symlink to a directory indexed nothing: Run follows the
// link with os.Stat and enters the directory branch, but filepath.WalkDir Lstats
// its root, so the callback saw a link and dropped it. The change signal
// (NewestDocMtime) had the same hole, so doc-sync never even scheduled the work.
func TestDocIngest_IndexesADirectorySourceThatIsASymlinkLos07e(t *testing.T) {
	store := mustOpenStore(t)

	base := t.TempDir()
	target := filepath.Join(base, "docs")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	modTime := time.Now().Add(-2 * time.Hour)
	if err := os.WriteFile(filepath.Join(target, "guide.md"), []byte("# Guide\n\nProse behind a symlinked source."), 0644); err != nil {
		t.Fatalf("write guide: %v", err)
	}

	link := filepath.Join(base, "docs-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported here: %v", err)
	}

	result, err := Run(Config{Project: "test-proj", Name: "linked-docs"}, []string{link}, store)
	if err != nil {
		t.Fatalf("ingest of a symlinked directory source: %v", err)
	}
	if result.FilesProcessed != 1 {
		t.Fatalf("processed = %d, want 1 document behind the symlink", result.FilesProcessed)
	}
	src, err := store.GetDocSource("linked-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := store.GetDocChunksByFile("guide.md", src.ID); len(chunks) == 0 {
		t.Error("no chunks stored for guide.md behind the symlinked source")
	}

	newest, ok := NewestDocMtime(link, time.Now())
	if !ok {
		t.Error("NewestDocMtime found no document behind the symlinked source, so doc-sync never schedules it")
	} else if newest.Before(modTime) {
		t.Errorf("newest = %v, want at least the document mtime %v", newest, modTime)
	}
}

// A path that carries a newline makes the OS error text split the log line: the
// quoted path was safe, the unquoted error was not (log-injection class).
func TestDocIngest_LogLineQuotesPathFromTheErrorLos07e(t *testing.T) {
	store := mustOpenStore(t)

	var buf bytes.Buffer
	prevWriter, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})

	dir := t.TempDir()
	missing := filepath.Join(dir, "bad\nFORGED.md")
	if _, err := Run(Config{Project: "test-proj", Name: "log-docs"}, []string{missing}, store); err != nil {
		t.Fatalf("a missing path aborted the run: %v", err)
	}

	if strings.Contains(buf.String(), "\nFORGED") {
		t.Errorf("log output carries a raw newline from the path, a forged line can pass as log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Skipped source") {
		t.Fatalf("the skipped source was not logged at all:\n%s", buf.String())
	}
}

// The whole-tree-gone case is the same loss the sweep handles for a
// subdirectory: a source whose documents are all deleted kept every chunk and
// the pass then recorded a complete sync, so the ghosts were permanent.
func TestDocIngest_SweepsChunksOfAnEmptiedDirectoryLos07e(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	doc := filepath.Join(dir, "only.md")
	if err := os.WriteFile(doc, []byte("# Only\n\nLast document of the source."), 0644); err != nil {
		t.Fatalf("write only: %v", err)
	}
	cfg := Config{Project: "test-proj", Name: "emptied-docs"}
	if _, err := Run(cfg, []string{dir}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("emptied-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := store.GetDocChunksByFile("only.md", src.ID); len(chunks) == 0 {
		t.Fatal("first ingest did not index only.md")
	}

	if err := os.Remove(doc); err != nil {
		t.Fatalf("remove only.md: %v", err)
	}
	result, err := Run(cfg, []string{dir}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile("only.md", src.ID); len(chunks) != 0 {
		t.Errorf("%d chunk(s) of a document survived although the whole source is empty now", len(chunks))
	}
	if result.FilesRemoved != 1 {
		t.Errorf("files removed = %d, want 1", result.FilesRemoved)
	}
}

// A single-file source whose file is gone keeps its chunks forever: doc-sync
// skips a missing path, so nothing else ever visits that source again.
func TestDocIngest_SweepsChunksOfADeletedSingleFileSourceLos07e(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	doc := filepath.Join(dir, "single.md")
	if err := os.WriteFile(doc, []byte("# Single\n\nA source that is one file."), 0644); err != nil {
		t.Fatalf("write single: %v", err)
	}
	cfg := Config{Project: "test-proj", Name: "single-docs"}
	if _, err := Run(cfg, []string{doc}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("single-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	// A single-file source stores its one document under ".", not under a name.
	if chunks, _ := store.GetDocChunksByFile(".", src.ID); len(chunks) == 0 {
		t.Fatal("first ingest did not index the single file")
	}

	if err := os.Remove(doc); err != nil {
		t.Fatalf("remove single.md: %v", err)
	}
	result, err := Run(cfg, []string{doc}, store)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile(".", src.ID); len(chunks) != 0 {
		t.Errorf("%d chunk(s) of the deleted single-file source survived", len(chunks))
	}
	if result.FilesRemoved != 1 {
		t.Errorf("files removed = %d, want 1", result.FilesRemoved)
	}
}

// Doc-sync ticks and the ingest handler write to the same source from different
// goroutines. The sweep's file set comes from its own walk, so a document that
// a concurrent pass indexed after that walk must not be swept: only rows that
// existed before the walk started are candidates.
func TestDocIngest_SweepLeavesChunksAboveTheWatermarkLos07e(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A\n\nProse."), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if _, err := Run(Config{Project: "test-proj", Name: "race-docs"}, []string{dir}, store); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	src, err := store.GetDocSource("race-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	chunks, err := store.GetDocChunksByFile("a.md", src.ID)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("chunks of a.md: %v (n=%d)", err, len(chunks))
	}

	// A walk that never saw a.md (as if it appeared mid-run), with the watermark
	// from before it was indexed: the chunks are newer than the walk, so they stay.
	result := &Result{}
	if err := sweepRemovedDocuments(store, src.ID, map[string]bool{}, src.Path, map[string]bool{src.Path: true}, chunks[0].ID-1, result); err != nil {
		t.Fatalf("sweep with an older watermark: %v", err)
	}
	if left, _ := store.GetDocChunksByFile("a.md", src.ID); len(left) == 0 {
		t.Error("the sweep deleted chunks that were written after its watermark")
	}
	if result.FilesRemoved != 0 {
		t.Errorf("files removed = %d, want 0 above the watermark", result.FilesRemoved)
	}

	// The same walk with a watermark above the rows is a real deletion.
	if err := sweepRemovedDocuments(store, src.ID, map[string]bool{}, src.Path, map[string]bool{src.Path: true}, chunks[len(chunks)-1].ID, result); err != nil {
		t.Fatalf("sweep with a current watermark: %v", err)
	}
	if left, _ := store.GetDocChunksByFile("a.md", src.ID); len(left) != 0 {
		t.Errorf("%d chunk(s) below the watermark survived", len(left))
	}
}

// A dry run must not write. Registering the source would move its row (and with
// last_sync at the epoch it would schedule a real ingest on the next tick) for a
// pass that indexed nothing.
func TestDocIngest_DryRunDoesNotRegisterTheSourceLos07e(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	doc := filepath.Join(dir, "preview.md")
	if err := os.WriteFile(doc, []byte("# Preview\n\nProse."), 0644); err != nil {
		t.Fatalf("write preview: %v", err)
	}

	result, err := Run(Config{Project: "test-proj", Name: "preview-docs", DryRun: true}, []string{dir}, store)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if result.FilesProcessed != 1 {
		t.Errorf("processed = %d, want 1 in a dry run", result.FilesProcessed)
	}
	if src, err := store.GetDocSource("preview-docs", "test-proj"); err == nil && src != nil {
		t.Errorf("a dry run registered the source %q", src.Name)
	}

	// And on an existing source it must leave the sync stamp alone.
	if _, err := Run(Config{Project: "test-proj", Name: "preview-docs"}, []string{dir}, store); err != nil {
		t.Fatalf("real ingest: %v", err)
	}
	const oldStamp = "2020-01-01 00:00:00"
	if _, err := store.DB().Exec(`UPDATE doc_sources SET last_sync = ? WHERE name = ?`, oldStamp, "preview-docs"); err != nil {
		t.Fatalf("age last_sync: %v", err)
	}
	if _, err := Run(Config{Project: "test-proj", Name: "preview-docs", DryRun: true}, []string{dir}, store); err != nil {
		t.Fatalf("second dry run: %v", err)
	}
	src, err := store.GetDocSource("preview-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if !src.LastSync.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a dry run moved last_sync to %v", src.LastSync)
	}
}

// The zero-file branch is the one place a dry run could still write: it stamps
// last_sync and, with that stamp, hides the loss the caller only previewed.
func TestDocIngest_DryRunOnAnEmptyTreeWritesNothingLos07e(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	doc := filepath.Join(dir, "preview.md")
	if err := os.WriteFile(doc, []byte("# Preview\n\nProse."), 0644); err != nil {
		t.Fatalf("write preview: %v", err)
	}
	cfg := Config{Project: "test-proj", Name: "empty-preview-docs"}
	if _, err := Run(cfg, []string{dir}, store); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := os.Remove(doc); err != nil {
		t.Fatalf("remove preview: %v", err)
	}
	const oldStamp = "2020-01-01 00:00:00"
	if _, err := store.DB().Exec(`UPDATE doc_sources SET last_sync = ? WHERE name = ?`, oldStamp, cfg.Name); err != nil {
		t.Fatalf("age last_sync: %v", err)
	}

	cfg.DryRun = true
	if _, err := Run(cfg, []string{dir}, store); err != nil {
		t.Fatalf("dry run on an empty tree: %v", err)
	}
	src, err := store.GetDocSource(cfg.Name, cfg.Project)
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if !src.LastSync.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a dry run moved last_sync to %v, so the loss it previewed is never swept", src.LastSync)
	}
	if chunks, _ := store.GetDocChunksByFile("preview.md", src.ID); len(chunks) == 0 {
		t.Error("a dry run deleted chunks")
	}
}

// A source re-registered under a narrower path keeps its chunk rows keyed on the
// OLD basePath. Sweeping against the new path would delete documents that still
// exist outside it.
func TestDocIngest_ReRegisteredSourcePathDoesNotSweepOutsideLos07e(t *testing.T) {
	store := mustOpenStore(t)

	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "top.md"), []byte("# Top\n\nProse at the root."), 0644); err != nil {
		t.Fatalf("write top: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inside.md"), []byte("# Inside\n\nProse in the subdirectory."), 0644); err != nil {
		t.Fatalf("write inside: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "moving-docs"}
	if _, err := Run(cfg, []string{root}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("moving-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := store.GetDocChunksByFile("top.md", src.ID); len(chunks) == 0 {
		t.Fatal("first ingest did not index top.md")
	}

	// Re-register the same source name against a subtree. top.md is outside the
	// new path but still on disk, so its chunks must survive.
	if _, err := Run(cfg, []string{sub}, store); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile("top.md", src.ID); len(chunks) == 0 {
		t.Error("the sweep deleted the chunks of a document that still exists outside the newly registered path")
	}
}

// A registration that names several paths stores the roots below the others as
// "../..." rows. The daemon re-runs every source with its registered path only,
// so that single-path run must not read those rows as deleted documents.
func TestDocIngest_MultiPathRowsAreNotSweptByASinglePathRunLos07e(t *testing.T) {
	store := mustOpenStore(t)

	root := t.TempDir()
	docs := filepath.Join(root, "docs")
	other := filepath.Join(root, "other")
	for _, dir := range []string{docs, other} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(docs, "a.md"), []byte("# A\n\nProse in the first root."), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(other, "b.md"), []byte("# B\n\nProse in the second root."), 0644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "multi-docs"}
	if _, err := Run(cfg, []string{docs, other}, store); err != nil {
		t.Fatalf("multi-path ingest: %v", err)
	}
	src, err := store.GetDocSource("multi-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	// The second root is stored relative to the first, so it escapes it.
	if chunks, _ := store.GetDocChunksByFile("../other/b.md", src.ID); len(chunks) == 0 {
		t.Fatal("the multi-path ingest did not index the second root")
	}

	if _, err := Run(cfg, []string{src.Path}, store); err != nil {
		t.Fatalf("single-path ingest: %v", err)
	}
	if chunks, _ := store.GetDocChunksByFile("../other/b.md", src.ID); len(chunks) == 0 {
		t.Error("a single-path run swept the chunks of a document outside its own base path")
	}
}

// The second pass over a symlinked source must remove nothing: the remapped
// entry names and the sweep's view of the tree have to agree.
func TestDocIngest_SecondRunOverASymlinkSourceRemovesNothingLos07e(t *testing.T) {
	store := mustOpenStore(t)

	base := t.TempDir()
	target := filepath.Join(base, "docs")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "guide.md"), []byte("# Guide\n\nProse."), 0644); err != nil {
		t.Fatalf("write guide: %v", err)
	}
	link := filepath.Join(base, "docs-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported here: %v", err)
	}

	cfg := Config{Project: "test-proj", Name: "linked-twice-docs"}
	for i := 0; i < 2; i++ {
		result, err := Run(cfg, []string{link}, store)
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if result.FilesRemoved != 0 {
			t.Fatalf("run %d removed %d document(s) of a tree that did not change", i+1, result.FilesRemoved)
		}
	}
	src, err := store.GetDocSource("linked-twice-docs", "test-proj")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := store.GetDocChunksByFile("guide.md", src.ID); len(chunks) == 0 {
		t.Error("the document behind the symlink lost its chunks")
	}
}

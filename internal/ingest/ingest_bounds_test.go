package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// YM-294 [DEV-DT46]: ingest read every source file with an unbounded
// io.ReadAll. The bound is a variable so this test can exercise it without a
// multi-megabyte fixture (same shape as internal/update/download.go).
func TestDocIngest_SkipsFilesAboveTheSizeBoundLos07c(t *testing.T) {
	store := mustOpenStore(t)

	previous := maxDocFileBytes
	maxDocFileBytes = 1024
	t.Cleanup(func() { maxDocFileBytes = previous })

	dir := t.TempDir()
	big := filepath.Join(dir, "huge.md")
	if err := os.WriteFile(big, []byte(strings.Repeat("# heading with prose\n\n", 200)), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if info, err := os.Stat(big); err != nil || info.Size() <= maxDocFileBytes {
		t.Fatalf("fixture precondition: size=%v err=%v, want more than %d", info, err, maxDocFileBytes)
	}

	result, err := Run(Config{Project: "test-proj", Name: "oversized-docs"}, []string{big}, store)
	if err != nil {
		t.Fatalf("ingest run: %v", err)
	}
	if result.FilesProcessed != 0 {
		t.Errorf("ingest processed %d file(s) above the %d byte bound", result.FilesProcessed, maxDocFileBytes)
	}
	if result.FilesSkipped != 1 {
		t.Errorf("skipped = %d, want 1: a refused document must not vanish from both counters", result.FilesSkipped)
	}

	ds, err := store.GetDocSource("oversized-docs", "test-proj")
	if err != nil || ds == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, ds)
	}
	if ds.ChunkCount != 0 {
		t.Errorf("an oversized file produced %d chunk(s), want 0", ds.ChunkCount)
	}
}

// YM-294 [DEV-DT46]: the walk dropped symlinks but kept every other
// non-regular entry. A FIFO named like a document is opened for reading and
// blocks forever, which stalls the ingest of the whole source.
func TestDocIngest_DoesNotBlockOnANamedPipeLos07c(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n\nRegular prose."), 0644); err != nil {
		t.Fatalf("write regular fixture: %v", err)
	}
	pipe := filepath.Join(dir, "blocking.md")
	if err := syscall.Mkfifo(pipe, 0644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := Run(Config{Project: "test-proj", Name: "pipe-docs"}, []string{dir}, store)
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("ingest run: %v", o.err)
		}
		if o.result.FilesProcessed != 1 {
			t.Errorf("processed %d files, want the one regular document", o.result.FilesProcessed)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("ingest blocked on a named pipe (%s) instead of skipping it", pipe)
	}
}

// A source path that is itself a named pipe has no document to read. It must be
// skipped instead of opened; a multi-path run must still index its good paths.
func TestDocIngest_SkipsANonRegularSourceFileLos07c(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	pipe := filepath.Join(dir, "direct.md")
	if err := syscall.Mkfifo(pipe, 0644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	good := filepath.Join(dir, "good.md")
	if err := os.WriteFile(good, []byte("# Good\n\nProse."), 0644); err != nil {
		t.Fatalf("write good fixture: %v", err)
	}

	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := Run(Config{Project: "test-proj", Name: "pipe-source"}, []string{pipe, good}, store)
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("ingest run: %v", o.err)
		}
		if o.result.FilesProcessed != 1 {
			t.Errorf("processed %d files, want only the regular document", o.result.FilesProcessed)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("ingest blocked on a named pipe source instead of skipping it")
	}
}

// A single-file source skipped the format gate the directory walk applies, so
// any absolute path was chunked as markdown.
func TestDocIngest_SkipsSingleFileWithUnsupportedExtensionLos07c(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(jsonFile, []byte(`{"a":1}`), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	result, err := Run(Config{Project: "test-proj", Name: "json-source"}, []string{jsonFile}, store)
	if err != nil {
		t.Fatalf("ingest run: %v", err)
	}
	if result.FilesProcessed != 0 {
		t.Errorf("ingest processed %q although its format is not indexable", jsonFile)
	}
	if result.FilesSkipped != 1 {
		t.Errorf("skipped = %d, want 1 for a non-indexable source", result.FilesSkipped)
	}
	// Nothing was collected, so no source row is created for it.
	if ds, err := store.GetDocSource("json-source", "test-proj"); err == nil && ds != nil {
		t.Errorf("a source with no indexable file was registered anyway: %+v", ds)
	}
}

// A path that no longer resolves must not cost a multi-path run the paths that
// are indexable. The stored source path anchors the relative chunk paths, so it
// has to come from a path that actually yielded files.
func TestDocIngest_SkipsAMissingPathLos07c(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	good := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(good, []byte("# Notes\n\nProse."), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	missing := filepath.Join(dir, "gone", "missing.md")

	result, err := Run(Config{Project: "test-proj", Name: "partial-docs"}, []string{missing, good}, store)
	if err != nil {
		t.Fatalf("a missing path aborted the whole run: %v", err)
	}
	if result.FilesProcessed != 1 {
		t.Errorf("processed %d files, want the one path that exists", result.FilesProcessed)
	}
	if result.FilesSkipped != 1 {
		t.Errorf("skipped = %d, want 1 for the missing path", result.FilesSkipped)
	}

	ds, err := store.GetDocSource("partial-docs", "test-proj")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}
	if ds == nil {
		t.Fatal("no source row for the run that indexed one file")
	}
	if ds.Path == missing {
		t.Errorf("source path anchored on the skipped path %q", ds.Path)
	}
	if ds.ChunkCount == 0 {
		t.Error("the existing path produced no chunks")
	}
}

// A zero-yield run refreshes the row only when that row's own path is the one
// that yielded nothing. Stamping an unrelated source would move its last_sync
// past a pending change, and doc-sync compares last_sync to the path's mtime:
// the change would never be indexed.
func TestDocIngest_ZeroYieldKeepsUnrelatedSourceSyncStampLos07c(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	good := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(good, []byte("# Notes\n\nProse."), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := Run(Config{Project: "test-proj", Name: "kept"}, []string{dir}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	const oldStamp = "2020-01-01 00:00:00"
	if _, err := store.DB().Exec(`UPDATE doc_sources SET last_sync = ? WHERE name = ?`, oldStamp, "kept"); err != nil {
		t.Fatalf("age last_sync: %v", err)
	}

	other := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(other, []byte(`{"a":1}`), 0644); err != nil {
		t.Fatalf("write other fixture: %v", err)
	}
	if _, err := Run(Config{Project: "test-proj", Name: "kept"}, []string{other}, store); err != nil {
		t.Fatalf("zero-yield ingest: %v", err)
	}

	ds, err := store.GetDocSource("kept", "test-proj")
	if err != nil || ds == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, ds)
	}
	if !ds.LastSync.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last_sync of an unrelated source moved to %v, want the aged stamp", ds.LastSync)
	}
}

// The other half: when the row's own path is the one that yielded nothing, the
// stamp is refreshed so doc-sync stops retrying it forever.
func TestDocIngest_ZeroYieldRefreshesItsOwnSourceStampLos07c(t *testing.T) {
	store := mustOpenStore(t)

	dir := t.TempDir()
	doc := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(doc, []byte("# Notes\n\nProse."), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := Run(Config{Project: "test-proj", Name: "emptied"}, []string{dir}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE doc_sources SET last_sync = ? WHERE name = ?`, "2020-01-01 00:00:00", "emptied"); err != nil {
		t.Fatalf("age last_sync: %v", err)
	}

	// The document disappears, so the source now yields nothing.
	if err := os.Remove(doc); err != nil {
		t.Fatalf("remove fixture: %v", err)
	}
	if _, err := Run(Config{Project: "test-proj", Name: "emptied"}, []string{dir}, store); err != nil {
		t.Fatalf("zero-yield ingest: %v", err)
	}

	ds, err := store.GetDocSource("emptied", "test-proj")
	if err != nil || ds == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, ds)
	}
	if ds.LastSync.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("last_sync of a source whose own path yielded nothing was not refreshed; doc-sync would retry it forever")
	}
}

// The bounded writer is what keeps a PDF text bomb from filling the daemon's
// memory: it must stop buffering at the limit and still accept every write.
func TestBoundedWriterStopsBufferingAtTheLimitLos07c(t *testing.T) {
	w := &boundedWriter{limit: 10}
	n, err := w.Write([]byte("0123456789abcdef"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 16 {
		t.Errorf("Write returned %d, want the full length so the child never sees a short write", n)
	}
	if !w.overrun {
		t.Error("overrun not recorded")
	}
	if got := w.buf.String(); got != "0123456789" {
		t.Errorf("buffered %q, want the bounded prefix %q", got, "0123456789")
	}
	// A second write past the limit must not grow the buffer either.
	if _, err := w.Write([]byte("more")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if w.buf.Len() != 10 {
		t.Errorf("buffer grew to %d after the overrun, want 10", w.buf.Len())
	}
}

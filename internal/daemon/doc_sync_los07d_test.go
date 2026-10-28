package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/ingest"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los07dChtimes moves a path's mtime, so a test can express "this file changed
// after the last sync" without sleeping.
func los07dChtimes(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// YM-324 [DEV-DT50]: hasDocSyncWork stat'ed the source path itself. For a
// directory source that mtime only moves when an entry is added or removed
// directly inside it, so an edit in a subdirectory left the gate at "no work"
// and the daemon never re-ingested it.
func TestHasDocSyncWork_SeesChangesInsideADirectorySourceLos07d(t *testing.T) {
	s := mustStore(t)

	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	doc := filepath.Join(sub, "changes.md")
	if err := os.WriteFile(doc, []byte("# Draft\n\nFirst version."), 0644); err != nil {
		t.Fatalf("write doc: %v", err)
	}

	if _, err := ingest.Run(ingest.Config{Project: "p", Name: "dir-docs"}, []string{dir}, s); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}

	// Everything older than the recorded last_sync: the gate must be quiet.
	past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	los07dChtimes(t, doc, past)
	los07dChtimes(t, sub, past)
	los07dChtimes(t, dir, past)
	if hasDocSyncWork(s) {
		t.Fatalf("precondition: nothing changed since the last sync, the gate must report no work")
	}

	// Only the document inside the subdirectory changes. Neither the
	// subdirectory nor the source directory is touched. The mtime sits at the
	// current clock: last_sync is truncated to whole seconds, so this is
	// strictly later than it without being ahead of the wall clock.
	los07dChtimes(t, doc, time.Now())

	if !hasDocSyncWork(s) {
		t.Fatal("a document changed inside the directory source but the gate still reports no work")
	}
}

// YM-324 [DEV-DT50] (the 07b remainder, needed for the directory walk above): the
// gate resolved a relative source path against the daemon's working directory and
// went permanently true for a tree that happens to share the name, while
// runDocSync skipped that source.
func TestHasDocSyncWork_IgnoresRelativeSourcePathLos07d(t *testing.T) {
	s := mustStore(t)

	decoy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(decoy, "los07d-reldocs"), 0755); err != nil {
		t.Fatalf("mkdir decoy: %v", err)
	}
	doc := filepath.Join(decoy, "los07d-reldocs", "decoy.md")
	if err := os.WriteFile(doc, []byte("# Decoy\n\nForeign content."), 0644); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	los07dChtimes(t, doc, time.Now())
	t.Chdir(decoy)

	if _, err := s.UpsertDocSource(&storage.DocSource{Name: "los07d-relative", Project: "p", Path: "los07d-reldocs"}); err != nil {
		t.Fatalf("upsert source: %v", err)
	}

	if hasDocSyncWork(s) {
		t.Fatal("a relative source path must not schedule work: the daemon's cwd is not the directory it was registered from")
	}
}

// YM-324: last_sync is CURRENT_TIMESTAMP, so a document dated ahead of the wall
// clock stays "after the last sync" on every tick. Treating it as work would run
// the full sync forever; it must count as a change only once the clock passes it.
func TestHasDocSyncWork_FutureMtimeDoesNotKeepTheGateOpenLos07d(t *testing.T) {
	s := mustStore(t)

	dir := t.TempDir()
	doc := filepath.Join(dir, "skewed.md")
	if err := os.WriteFile(doc, []byte("# Skewed\n\nContent from a host with a clock in the future."), 0644); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	if _, err := ingest.Run(ingest.Config{Project: "p", Name: "skewed-docs"}, []string{dir}, s); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}

	past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	los07dChtimes(t, doc, past)
	los07dChtimes(t, dir, past)
	if hasDocSyncWork(s) {
		t.Fatalf("precondition: nothing changed since the last sync, the gate must report no work")
	}

	los07dChtimes(t, doc, time.Now().Add(time.Hour))
	if hasDocSyncWork(s) {
		t.Fatal("a mtime ahead of the clock must not schedule work: it would keep the full sync running on every tick")
	}
}

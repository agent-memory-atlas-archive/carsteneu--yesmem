package indexer

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/bloom"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los04fForgedName is a session file name an agent harness can produce: the
// projects tree is written by the client, and a name may carry a newline.
const los04fForgedName = "sess\nwarn: forged log line.jsonl"

// los04fCaptureLog redirects the standard logger into a buffer for one test.
func los04fCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// los04fWriteFixture copies the parser sample session to path.
func los04fWriteFixture(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "sample-session.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// los04fAssertQuotedPathLog checks that the logged path stayed inside one line
// and was rendered with quoting, so no part of the file name can pose as its
// own log entry.
func los04fAssertQuotedPathLog(t *testing.T, logged string, path string) {
	t.Helper()
	if logged == "" {
		t.Fatal("no log line captured")
	}
	if got := strings.Count(logged, "\n"); got != 1 {
		t.Fatalf("log output spans %d lines, want 1 (a path newline forged an entry):\n%s", got, logged)
	}
	if !strings.Contains(logged, strconv.Quote(path)) {
		t.Fatalf("logged path is not quoted; log line: %s", logged)
	}
}

// A session file name is client input. Logging it with %s lets a name carrying
// a newline end the line early and pass the rest off as a separate entry
// (YM-311, escaped from YM-277).
func TestIndexSession_QuotesLoggedPathOnArchiveFailure(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	dir := t.TempDir()
	path := filepath.Join(dir, los04fForgedName)
	los04fWriteFixture(t, path)

	archiveDir := t.TempDir()
	// A plain file where the archive subdirectory belongs makes the archive step
	// fail after the session was parsed and stored, so the warn line is reached.
	blocker := filepath.Join(archiveDir, projectDirName(path))
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := New(store, bloom.New(), archive.New(archiveDir), nil)
	logged := los04fCaptureLog(t)

	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index session: %v", err)
	}
	los04fAssertQuotedPathLog(t, logged.String(), path)
}

// The archive error text carries the destination path, and that path ends on the
// client's file name. A raw path there splits the log line just as the raw
// source path did (found by the cold review of YM-311).
func TestIndexSession_ArchiveErrorTextStaysOnOneLogLine(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	dir := t.TempDir()
	path := filepath.Join(dir, los04fForgedName)
	los04fWriteFixture(t, path)
	// Grow the source beyond any directory size, so the archive step takes the
	// copy path against the directory that sits at the destination.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat(" ", 20000)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	archiveDir := t.TempDir()
	blocker := filepath.Join(archiveDir, projectDirName(path), filepath.Base(path))
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}

	idx := New(store, bloom.New(), archive.New(archiveDir), nil)
	logged := los04fCaptureLog(t)

	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index session: %v", err)
	}
	los04fAssertQuotedPathLog(t, logged.String(), path)
}

// The same class on the batch path: IndexAllWithProgress logs every failed file
// path (YM-311).
func TestIndexAllWithProgress_QuotesLoggedPath(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Closed on purpose: every store write fails, so the batch loop reports the
	// path of the failed file without the error text carrying a path itself.
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, los04fForgedName)
	los04fWriteFixture(t, path)

	idx := New(store, bloom.New(), archive.New(t.TempDir()), nil)
	logged := los04fCaptureLog(t)

	indexed, _, err := idx.IndexAllWithProgress(dir, nil)
	if err != nil {
		t.Fatalf("index all: %v", err)
	}
	if indexed != 0 {
		t.Fatalf("indexed = %d with a closed store, want 0", indexed)
	}
	los04fAssertQuotedPathLog(t, logged.String(), path)
}

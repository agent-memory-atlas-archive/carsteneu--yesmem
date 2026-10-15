package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// openTestStore returns a fresh in-memory store. In :memory: mode all logical
// databases (yesmem/messages/runtime) share one handle, so index_state and
// messages live in the same schema — good enough for storage-level tests.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIndexStateRoundtripsIncrementalMarkers(t *testing.T) {
	s := openTestStore(t)

	mtime := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	in := &models.IndexState{
		JSONLPath:        "/tmp/sess.jsonl",
		FileSize:         4096,
		FileMtime:        mtime,
		IndexedAt:        time.Date(2026, 10, 14, 10, 5, 0, 0, time.UTC),
		IndexedOffset:    4000,
		IndexedSessionID: "session-abc",
		IndexedMsgCount:  17,
	}
	if err := s.UpsertIndexState(in); err != nil {
		t.Fatalf("upsert index state: %v", err)
	}

	got, err := s.GetIndexState(in.JSONLPath)
	if err != nil {
		t.Fatalf("get index state: %v", err)
	}
	if got.IndexedOffset != 4000 {
		t.Errorf("IndexedOffset: got %d, want 4000", got.IndexedOffset)
	}
	if got.IndexedSessionID != "session-abc" {
		t.Errorf("IndexedSessionID: got %q, want session-abc", got.IndexedSessionID)
	}
	if got.IndexedMsgCount != 17 {
		t.Errorf("IndexedMsgCount: got %d, want 17", got.IndexedMsgCount)
	}
	if got.FileSize != 4096 {
		t.Errorf("FileSize: got %d, want 4096", got.FileSize)
	}
	if !got.FileMtime.Equal(mtime) {
		t.Errorf("FileMtime: got %v, want %v", got.FileMtime, mtime)
	}

	// Overwrite must replace the markers, not accumulate.
	in.IndexedOffset = 9000
	in.IndexedMsgCount = 22
	in.FileSize = 9096
	if err := s.UpsertIndexState(in); err != nil {
		t.Fatalf("re-upsert index state: %v", err)
	}
	got, err = s.GetIndexState(in.JSONLPath)
	if err != nil {
		t.Fatalf("get index state after update: %v", err)
	}
	if got.IndexedOffset != 9000 || got.IndexedMsgCount != 22 {
		t.Errorf("after update: offset=%d msgcount=%d, want 9000/22", got.IndexedOffset, got.IndexedMsgCount)
	}
}

func TestIndexStateMissingPathReturnsError(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.GetIndexState("/tmp/never-indexed.jsonl"); err == nil {
		t.Fatal("expected error for unknown path")
	}
	if !s.NeedsReindex("/tmp/never-indexed.jsonl", 10, time.Now()) {
		t.Error("unknown path must need reindex")
	}
}

// NeedsReindex must stay byte/mtime based: equal size+mtime is a no-op,
// a grown size (append) still reports reindex=true so the caller decides
// whether the append fast path applies.
func TestNeedsReindexOnGrownFile(t *testing.T) {
	s := openTestStore(t)

	mtime := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	if err := s.UpsertIndexState(&models.IndexState{
		JSONLPath: "/tmp/growing.jsonl", FileSize: 100, FileMtime: mtime, IndexedAt: mtime,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if s.NeedsReindex("/tmp/growing.jsonl", 100, mtime) {
		t.Error("unchanged file must not need reindex")
	}
	if !s.NeedsReindex("/tmp/growing.jsonl", 200, mtime) {
		t.Error("grown file must need reindex")
	}
	if !s.NeedsReindex("/tmp/growing.jsonl", 50, mtime) {
		t.Error("shrunk file must need reindex")
	}
}

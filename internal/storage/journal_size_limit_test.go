package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestJournalSizeLimit_TruncatesWalAfterCheckpoint verifies that the storage
// layer caps the SQLite WAL file size. Without `PRAGMA journal_size_limit=N`
// the WAL is recycled in place after a checkpoint and stays at its peak
// physical size, which has been observed to grow into hundreds of MB on
// long-running daemons and slows every external sqlite3-reader by ~8s per
// open. The fix sets journal_size_limit so SQLite truncates the WAL file down
// to that limit when a connection carrying the setting restarts the log, which
// is the next write after a fully checkpointed WAL.
func TestJournalSizeLimit_TruncatesWalAfterCheckpoint(t *testing.T) {
	const walLimit = 10 * 1024 * 1024 // 10 MB, must match openSQLite() value

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "yesmem.db")
	walPath := dbPath + "-wal"

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	db := s.DB()

	// Disable auto-checkpoint so the WAL grows past the limit deterministically
	// regardless of the default 1000-page (~4MB) auto-checkpoint trigger.
	if _, err := db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatalf("disable autocheckpoint: %v", err)
	}

	if _, err := db.Exec(`CREATE TABLE wal_bulk (id INTEGER PRIMARY KEY, payload BLOB)`); err != nil {
		t.Fatalf("create bulk table: %v", err)
	}

	// Push the WAL well past the 10MB limit: 250 rows × 100KB = ~25MB raw,
	// which lands ~20-25MB on disk after framing.
	blob := make([]byte, 100*1024)
	for i := range blob {
		blob[i] = byte(i % 251)
	}
	for i := 0; i < 250; i++ {
		if _, err := db.Exec(`INSERT INTO wal_bulk (payload) VALUES (?)`, blob); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	walPre, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal pre-checkpoint: %v", err)
	}
	if walPre.Size() <= walLimit {
		t.Fatalf("test setup invalid: WAL only %d bytes pre-checkpoint, expected > %d", walPre.Size(), walLimit)
	}

	// journal_size_limit only kicks in on a WAL *reset*, which happens inside
	// walRestartLog when the next write transaction starts after the WAL has
	// been fully checkpointed. So: PASSIVE checkpoint to backfill all frames,
	// then a tiny commit to trigger the reset and the limit-driven truncation.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO wal_bulk (payload) VALUES (?)`, []byte("trigger")); err != nil {
		t.Fatalf("trigger insert: %v", err)
	}

	walPost, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal post-checkpoint: %v", err)
	}
	if walPost.Size() > walLimit {
		t.Fatalf("WAL not truncated by journal_size_limit: %d bytes after checkpoint, expected <= %d", walPost.Size(), walLimit)
	}
}

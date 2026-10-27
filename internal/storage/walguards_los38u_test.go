package storage

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

// The tests in this file pin the three WAL-guard rest points of los-38t and the
// safety boundary of the two pragmas that los-38t left unchecked (YM-355):
// journal_size_limit and mmap_size in the presence of an external writer.
//
// The store fixtures come from walhealth_los38t_test.go (los38tOpenStore,
// los38tLearning, los38tPragmaInt): a rename or removal of that file breaks
// this one.

// los38uExternal opens a second write connection to the same database file with
// the driver defaults, the way an external tool (sqlite3 CLI, a maintenance
// script) would. It is a separate pool, so it does not share a connection with
// the store; only the file and the -shm state are shared. busyTimeout bounds how
// long it waits for a lock, so a test that expects a refusal does not spend
// thirty seconds on it.
func los38uExternal(t *testing.T, path, busyTimeout string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path, dsnReadWrite, []pragmaSetting{{"busy_timeout", busyTimeout}}))
	if err != nil {
		t.Fatalf("open external writer: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	return db
}

// los38uProbeTable creates a minimal table for the external writer. The real
// schema is not needed here; the point is the file-level interaction.
func los38uProbeTable(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec("CREATE TABLE los38u_probe (id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
}

// los38uFill writes n rows with the given payload in one transaction.
func los38uFill(t *testing.T, db *sql.DB, n int, payload []byte) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO los38u_probe (v) VALUES (?)")
	if err != nil {
		tx.Rollback()
		t.Fatalf("prepare: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(payload); err != nil {
			stmt.Close()
			tx.Rollback()
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// los38uCount counts the probe rows through db.
func los38uCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM los38u_probe").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// los38uWalSize returns the WAL size, or -1 when the file is absent.
func los38uWalSize(path string) int64 {
	if fi, err := os.Stat(path + "-wal"); err == nil {
		return fi.Size()
	}
	return -1
}

// los38uCheckpoint runs a checkpoint and returns the three columns SQLite
// reports instead of an error: busy (1 when the mode could not be completed),
// the number of frames in the log and the number backfilled.
func los38uCheckpoint(t *testing.T, db *sql.DB, mode string) (busy, logFrames, checkpointed int) {
	t.Helper()
	if err := db.QueryRow("PRAGMA wal_checkpoint("+mode+")").Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatalf("wal_checkpoint(%s): %v", mode, err)
	}
	return busy, logFrames, checkpointed
}

// TestLos38uReadOnlyRefusesNonFileNames pins rest point (2): a read-only handle
// cannot promise mode=ro for a name that is not a file path, so it refuses the
// name instead of returning a handle whose read-only property is a naming
// convention. Without the guard the handle would open and only PRAGMA
// query_only would be missing.
func TestLos38uReadOnlyRefusesNonFileNames(t *testing.T) {
	// t.Chdir changes the working directory for the whole test process; the
	// package has no t.Parallel() today, so a relative path is safe here.
	t.Chdir(t.TempDir())
	for _, name := range []string{":memory:", "file:los38u.db?mode=ro"} {
		db, err := openSQLiteReadOnly(name)
		if err == nil {
			db.Close()
			t.Fatalf("openSQLiteReadOnly(%q) returned a handle", name)
		}
		if !strings.Contains(err.Error(), "needs a file path") {
			t.Errorf("openSQLiteReadOnly(%q) error = %v, want the file-path refusal", name, err)
		}
		if _, err := OpenReadOnly(name); err == nil {
			t.Fatalf("OpenReadOnly(%q) returned a store", name)
		}
	}
	if _, err := os.Stat("los38u.db"); !os.IsNotExist(err) {
		t.Fatalf("the refused open created los38u.db (stat err = %v)", err)
	}
}

// TestLos38uReadOnlyInheritsTheWritersJournalMode pins rest point (3): the
// journal mode is a property of the file that the write path sets, and a
// read-only handle reports it but cannot switch it. The asymmetry is the reason
// journal_mode is its own statement in openSQLite and absent from
// openSQLiteReadOnly.
func TestLos38uReadOnlyInheritsTheWritersJournalMode(t *testing.T) {
	s, path := los38tOpenStore(t)
	if _, err := s.InsertLearning(los38tLearning(1)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { ro.Close() })

	mode, err := journalModeOf(ro.db)
	if err != nil {
		t.Fatalf("read-only journal mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("read-only handle reports journal mode %q, want \"wal\"", mode)
	}
	if _, err := ro.db.Exec("PRAGMA journal_mode=DELETE"); err == nil {
		t.Fatalf("a read-only handle switched the journal mode of the database file")
	}
	if mode, err := journalModeOf(s.db); err != nil {
		t.Fatalf("writer journal mode: %v", err)
	} else if mode != "wal" {
		t.Fatalf("writer journal mode changed to %q after the read-only attempt", mode)
	}
}

// TestLos38uMessagesReadPoolKeepsWriterPragmas pins rest point (1) where it can
// regress: the messages pool pair. read_uncommitted was removed from the read
// pool (it has no effect in WAL), and the writer pragmas have to reach that
// pool through the DSN, so a pool without read_uncommitted still gets its busy
// wait, its WAL cap and its page cache.
func TestLos38uMessagesReadPoolKeepsWriterPragmas(t *testing.T) {
	s, _ := los38tOpenStore(t)
	if s.messagesReadDB == nil || s.messagesDB == nil {
		t.Fatalf("the store has no messages pool pair")
	}
	for _, c := range []struct {
		name string
		db   *sql.DB
	}{{"messages write", s.messagesDB}, {"messages read", s.messagesReadDB}} {
		if got := los38tPragmaInt(t, c.db, "busy_timeout"); got != 30000 {
			t.Errorf("%s: busy_timeout = %d, want 30000", c.name, got)
		}
		if got := los38tPragmaInt(t, c.db, "journal_size_limit"); got != 10485760 {
			t.Errorf("%s: journal_size_limit = %d, want 10485760", c.name, got)
		}
		if got := los38tPragmaInt(t, c.db, "mmap_size"); got != 268435456 {
			t.Errorf("%s: mmap_size = %d, want 268435456", c.name, got)
		}
		if got := los38tPragmaInt(t, c.db, "read_uncommitted"); got != 0 {
			t.Errorf("%s: read_uncommitted = %d, want 0 (it has no effect in WAL and hid writes from the read pool)", c.name, got)
		}
	}
}

// TestLos38uExternalWriterKeepsOurPoolsReadable covers the read side of rest
// point (4): an external writer appends through its own connection and then
// truncates the log. Rows, integrity and further writes stay correct on every
// pool the guard watches. The size of the log is not the subject here, only
// that its disappearance does not cost the store a read.
func TestLos38uExternalWriterKeepsOurPoolsReadable(t *testing.T) {
	s, path := los38tOpenStore(t)
	los38uProbeTable(t, s)
	los38uFill(t, s.db, 50, []byte("own"))
	if busy, _, _ := los38uCheckpoint(t, s.db, "TRUNCATE"); busy != 0 {
		t.Fatalf("checkpoint before the external write reports busy=%d", busy)
	}

	ext := los38uExternal(t, path, "30000")
	los38uFill(t, ext, 20000, []byte("external"))
	if got := los38uWalSize(path); got <= 0 {
		t.Fatalf("the external writer left no WAL (%d bytes)", got)
	}
	los38uCheckpoint(t, ext, "TRUNCATE")

	for _, c := range []struct {
		name string
		db   *sql.DB
	}{{"writer", s.db}, {"reader", s.ReaderDB()}, {"read-only", s.readOnlyDB}} {
		if got := los38uCount(t, c.db); got != 20050 {
			t.Errorf("%s: count after the external checkpoint = %d, want 20050", c.name, got)
		}
		var ok string
		if err := c.db.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil {
			t.Errorf("%s: integrity_check: %v", c.name, err)
		} else if ok != "ok" {
			t.Errorf("%s: integrity_check = %q", c.name, ok)
		}
	}
	if _, err := s.db.Exec("INSERT INTO los38u_probe (v) VALUES ('after')"); err != nil {
		t.Fatalf("write after the external checkpoint: %v", err)
	}
	if got := los38uCount(t, s.db); got != 20051 {
		t.Fatalf("count after our own insert = %d, want 20051", got)
	}
}

// TestLos38uOurNextWriteCapsTheExternalWAL pins the other half of
// journal_size_limit: the cap belongs to the connection that restarts the log.
// An external writer without the setting leaves a WAL of any size behind, and
// our next write truncates it to the limit. Removing the pragma turns this test
// red; a value that is too small only shows up in the pragma assertions above.
func TestLos38uOurNextWriteCapsTheExternalWAL(t *testing.T) {
	const walLimit = 10 * 1024 * 1024
	s, path := los38tOpenStore(t)
	los38uProbeTable(t, s)

	ext := los38uExternal(t, path, "30000")
	if _, err := ext.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatalf("disable autocheckpoint on the external writer: %v", err)
	}
	los38uFill(t, ext, 300, []byte(strings.Repeat("z", 100*1024)))
	before := los38uWalSize(path)
	if before <= walLimit {
		t.Fatalf("test setup invalid: external WAL is %d bytes, want more than %d", before, walLimit)
	}

	// A checkpoint backfills the frames but does not truncate; the cap only acts
	// when the log restarts, and that restart happens in the next write.
	if busy, _, _ := los38uCheckpoint(t, s.db, "RESTART"); busy != 0 {
		t.Fatalf("checkpoint(RESTART) reports busy=%d", busy)
	}
	if got := los38uWalSize(path); got <= walLimit {
		t.Fatalf("checkpoint(RESTART) already truncated the WAL to %d bytes, the test would not see the write", got)
	}
	if _, err := s.db.Exec("INSERT INTO los38u_probe (v) VALUES ('restart')"); err != nil {
		t.Fatalf("write that restarts the log: %v", err)
	}
	after := los38uWalSize(path)
	if after > walLimit {
		t.Fatalf("WAL after our write = %d bytes, want at most %d (journal_size_limit did not cap the external log)", after, walLimit)
	}
	if got := los38uCount(t, s.db); got != 301 {
		t.Fatalf("count after the capped restart = %d, want 301", got)
	}
}

// TestLos38uExternalCheckpointWaitsForOurReader covers the ordering the cap
// depends on: a TRUNCATE checkpoint needs the log restart, and the restart needs
// every read mark at the end of the log. While our snapshot is open the
// checkpoint reports busy and leaves the log in place; after the reader closes
// the same call truncates it. Asserting on the busy column matters: the call
// returns no error either way.
func TestLos38uExternalCheckpointWaitsForOurReader(t *testing.T) {
	s, path := los38tOpenStore(t)
	los38uProbeTable(t, s)
	los38uFill(t, s.db, 10, []byte("own"))
	los38uCheckpoint(t, s.db, "TRUNCATE")

	ctx := context.Background()
	tx, err := s.ReaderDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read transaction: %v", err)
	}
	var before int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM los38u_probe").Scan(&before); err != nil {
		tx.Rollback()
		t.Fatalf("read inside the transaction: %v", err)
	}
	if before != 10 {
		tx.Rollback()
		t.Fatalf("snapshot count = %d, want 10", before)
	}

	ext := los38uExternal(t, path, "200")
	los38uFill(t, ext, 5000, []byte("external"))
	busy, _, _ := los38uCheckpoint(t, ext, "TRUNCATE")
	if busy != 1 {
		tx.Rollback()
		t.Fatalf("TRUNCATE under our read mark reports busy=%d, want 1 (the snapshot must hold the log)", busy)
	}
	if got := los38uWalSize(path); got <= 0 {
		tx.Rollback()
		t.Fatalf("the external checkpoint truncated the WAL to %d bytes while our reader held a read mark", got)
	}

	// The open snapshot must survive: same count, no I/O error.
	var during int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM los38u_probe").Scan(&during); err != nil {
		tx.Rollback()
		t.Fatalf("second read inside the transaction (short read?): %v", err)
	}
	if during != before {
		tx.Rollback()
		t.Fatalf("snapshot changed under the external checkpoint: %d -> %d", before, during)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit read transaction: %v", err)
	}

	// With no reader holding frames the same call may truncate.
	if busy, _, _ := los38uCheckpoint(t, ext, "TRUNCATE"); busy != 0 {
		t.Fatalf("TRUNCATE after the reader closed reports busy=%d, want 0", busy)
	}
	if got := los38uWalSize(path); got != 0 {
		t.Fatalf("WAL after the released truncate = %d bytes, want 0", got)
	}
	if got := los38uCount(t, s.ReaderDB()); got != 5010 {
		t.Fatalf("count after the reader closed = %d, want 5010", got)
	}
}

// TestLos38uExternalRewriteKeepsOurReadsCorrect covers the mmap_size side of
// rest point (4) for the case that occurs: an external writer appends,
// checkpoints and thereby rewrites the database file while our pool holds a
// memory map. Reading through that map must return the committed data. This
// test cannot cover the counter-case, a foreign truncation of the database file
// itself: it does not survive mmap (SIGBUS, measured 19.10.2026) and a crash
// cannot be asserted in-process. The pragma comment names the boundary.
func TestLos38uExternalRewriteKeepsOurReadsCorrect(t *testing.T) {
	s, path := los38tOpenStore(t)
	if got := los38tPragmaInt(t, s.db, "mmap_size"); got != 268435456 {
		t.Fatalf("mmap_size on the writer pool = %d, want 268435456 (the case this test covers)", got)
	}
	los38uProbeTable(t, s)
	los38uFill(t, s.db, 200, []byte("own"))
	los38uCheckpoint(t, s.db, "TRUNCATE")
	// Populate the writer's mapping before the file is rewritten. The reader
	// pool maps lazily, so its map is cold on purpose: that is the interesting
	// half, a fresh mapping over a file an external writer just rewrote. The
	// read-only pool carries no mmap_size at all (readerPragmas) and reads with
	// pread; it is in the loop because a read path must not depend on the map.
	if got := los38uCount(t, s.db); got != 200 {
		t.Fatalf("count before the external writer = %d, want 200", got)
	}

	ext := los38uExternal(t, path, "30000")
	los38uFill(t, ext, 5000, []byte("external"))
	los38uCheckpoint(t, ext, "TRUNCATE")

	for _, c := range []struct {
		name string
		db   *sql.DB
	}{{"writer", s.db}, {"reader", s.ReaderDB()}, {"read-only", s.readOnlyDB}} {
		if got := los38uCount(t, c.db); got != 5200 {
			t.Errorf("%s: count through the memory map = %d, want 5200", c.name, got)
		}
	}
	var ok string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ok != "ok" {
		t.Fatalf("integrity_check = %q", ok)
	}
}

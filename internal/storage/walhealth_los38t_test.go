package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los38tOpenStore opens a store in a temp directory and returns it with the main
// database path.
func los38tOpenStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "yesmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func los38tLearning(i int) *models.Learning {
	return &models.Learning{Content: fmt.Sprintf("los38t row %d", i), Category: "gotcha", Source: "claude_suggested", Project: "/tmp/los38t"}
}

// los38tPrepare tail-writes so the WAL holds a frame that is not in the database
// file yet, then cuts the WAL to the requested length in place. An in-place cut
// keeps the inode, so the deleted-descriptor alarm stays silent and only the
// canary can notice.
func los38tPrepare(t *testing.T, s *Store, dbPath string, cut int64) {
	t.Helper()
	for i := 0; i < 20; i++ {
		if _, err := s.InsertLearning(los38tLearning(i)); err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
	}
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.InsertLearning(los38tLearning(90 + i)); err != nil {
			t.Fatalf("tail insert %d: %v", i, err)
		}
	}
	if err := os.Truncate(dbPath+"-wal", cut); err != nil {
		t.Fatalf("truncate wal: %v", err)
	}
}

// TestLos38tCanaryDetectsEveryTruncation pins the detection: an in-place cut of
// the WAL breaks a fresh connection for every length, because the wal-index of
// the surviving -shm still claims frames the log no longer holds.
func TestLos38tCanaryDetectsEveryTruncation(t *testing.T) {
	for _, cut := range []int64{0, 5, 32, 1024} {
		t.Run(fmt.Sprintf("%d_bytes", cut), func(t *testing.T) {
			s, path := los38tOpenStore(t)
			if err := walCanary(path); err != nil {
				t.Fatalf("canary on the healthy database: %v", err)
			}
			los38tPrepare(t, s, path, cut)
			err := walCanary(path)
			if err == nil {
				t.Fatalf("canary read a WAL cut to %d bytes without an error", cut)
			}
			if !isIOError(err) {
				t.Fatalf("canary error is %v, want the SQLITE_IOERR family", err)
			}
		})
	}
}

// TestLos38tGuardTickHealsWithoutRestart is the campaign regression test: after a
// foreign party cuts the WAL, the guard heals the store in-process, and fresh
// connections serve reads and writes again. No file is deleted, and the frames
// that were only in the log survive the close of the old connections.
func TestLos38tGuardTickHealsWithoutRestart(t *testing.T) {
	s, path := los38tOpenStore(t)
	los38tPrepare(t, s, path, 5)

	if err := walCanary(path); err == nil {
		t.Fatalf("canary read the poisoned database without an error")
	}

	s.walGuardTick()

	if err := walCanary(path); err != nil {
		t.Fatalf("canary still fails after the guard tick: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM learnings").Scan(&n); err != nil {
		t.Fatalf("read after heal: %v", err)
	}
	// The in-place cut destroyed the tail frames before the guard could act;
	// what the last checkpoint wrote stays. The unlink variant below keeps the
	// inode and therefore the frames.
	if n != 20 {
		t.Fatalf("row count after heal = %d, want 20 (what the last checkpoint holds)", n)
	}
	if err := s.ReaderDB().QueryRow("SELECT COUNT(*) FROM learnings").Scan(&n); err != nil {
		t.Fatalf("read pool after heal: %v", err)
	}
	if _, err := s.InsertLearning(los38tLearning(200)); err != nil {
		t.Fatalf("write after heal: %v", err)
	}
}

// TestLos38tGuardHealsUnlinkedSidecars covers the production observation: the
// sidecar files were unlinked while the daemon held them. The alarm must name
// them and the store must recover.
func TestLos38tGuardHealsUnlinkedSidecars(t *testing.T) {
	s, path := los38tOpenStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.InsertLearning(los38tLearning(i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil {
			t.Fatalf("remove %s: %v", suffix, err)
		}
	}

	s.walGuardTick()

	if err := walCanary(path); err != nil {
		t.Fatalf("canary fails after healing unlinked sidecars: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM learnings").Scan(&n); err != nil {
		t.Fatalf("read after heal: %v", err)
	}
	if n != 3 {
		t.Fatalf("row count after heal = %d, want 3", n)
	}
}

// TestLos38tHealKeepsConnectionPragmas guards the heal's side effect on the
// pools: after its connections are dropped, a pool must come back with the same
// per-connection settings, not with the driver defaults.
func TestLos38tHealKeepsConnectionPragmas(t *testing.T) {
	s, _ := los38tOpenStore(t)
	if _, err := s.InsertLearning(los38tLearning(1)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	drop := dropPooledConnections(s.walGuardPools()[0].all)
	drop()

	for _, c := range []struct {
		name string
		db   *sql.DB
	}{{"writer", s.db}, {"reader", s.ReaderDB()}, {"read-only", s.readOnlyDB}} {
		if got := los38tPragmaInt(t, c.db, "busy_timeout"); got != 30000 {
			t.Errorf("%s: busy_timeout = %d, want 30000", c.name, got)
		}
	}
	for _, c := range []struct {
		name string
		db   *sql.DB
	}{{"writer", s.db}, {"reader", s.ReaderDB()}} {
		if got := los38tPragmaInt(t, c.db, "journal_size_limit"); got != 10485760 {
			t.Errorf("%s: journal_size_limit = %d, want 10485760", c.name, got)
		}
		if got := los38tPragmaInt(t, c.db, "mmap_size"); got != 268435456 {
			t.Errorf("%s: mmap_size = %d, want 268435456", c.name, got)
		}
	}
	if got := los38tPragmaInt(t, s.readOnlyDB, "query_only"); got != 1 {
		t.Fatalf("read-only pool: query_only = %d after the drop, want 1", got)
	}
	if _, err := s.readOnlyDB.Exec("CREATE TABLE los38t_should_fail (x)"); err == nil {
		t.Fatalf("read-only pool accepted a write after its connection was replaced")
	}
}

// TestLos38tGuardPoolsGroupByFile pins the grouping the heal depends on: every
// pool that shares a file has to be dropped together, or the inode state stays
// pinned.
func TestLos38tGuardPoolsGroupByFile(t *testing.T) {
	s, path := los38tOpenStore(t)
	pools := s.walGuardPools()
	if len(pools) != 3 {
		t.Fatalf("guard watches %d pools, want 3", len(pools))
	}
	byName := map[string]dbPool{}
	for _, p := range pools {
		byName[p.name] = p
	}
	main, ok := byName["yesmem.db"]
	if !ok {
		t.Fatalf("no pool for yesmem.db: %v", byName)
	}
	if main.path != resolveStorePath(path) {
		t.Errorf("main pool path = %q, want %q", main.path, resolveStorePath(path))
	}
	if len(main.all) != 3 {
		t.Errorf("yesmem.db pool holds %d handles, want 3 (db, read, read-only)", len(main.all))
	}
	messages := byName["messages.db"]
	if len(messages.all) != 2 {
		t.Errorf("messages.db pool holds %d handles, want 2 (write, read)", len(messages.all))
	}
	if len(byName["runtime.db"].all) != 1 {
		t.Errorf("runtime.db pool holds %d handles, want 1", len(byName["runtime.db"].all))
	}
}

// TestLos38tGuardWorksThroughSymlinkedDataDir covers a data directory that is a
// symlink: readlink reports the resolved path, so the guard has to compare
// resolved paths or the whole heal path stays dead.
func TestLos38tGuardWorksThroughSymlinkedDataDir(t *testing.T) {
	// The sidecar scan reads /proc/<pid>/fd, which darwin does not expose.
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("the sidecar scan needs /proc/<pid>/fd")
	}
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	s, err := Open(filepath.Join(link, "yesmem.db"))
	if err != nil {
		t.Fatalf("Open through symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.InsertLearning(los38tLearning(1)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	realPath := filepath.Join(real, "yesmem.db")
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(realPath + suffix); err != nil {
			t.Fatalf("remove %s: %v", suffix, err)
		}
	}

	if deleted := deletedSidecarFDs(s.walGuardPaths()); len(deleted) == 0 {
		t.Fatalf("the guard did not see the unlinked sidecars behind the symlinked data dir")
	}

	s.walGuardTick()

	if err := walCanary(realPath); err != nil {
		t.Fatalf("canary fails after healing through the symlinked data dir: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM learnings").Scan(&n); err != nil {
		t.Fatalf("read after heal: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count after heal = %d, want 1", n)
	}
}

// TestLos38tGuardLeavesHealthyDatabaseAlone runs ticks against a database under
// write load: no heal, no data change, no WAL loss, and no false alarm.
func TestLos38tGuardLeavesHealthyDatabaseAlone(t *testing.T) {
	s, path := los38tOpenStore(t)
	for i := 0; i < 40; i++ {
		if _, err := s.InsertLearning(los38tLearning(i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	before, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}

	for i := 0; i < 3; i++ {
		s.walGuardTick()
	}

	after, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("stat wal after ticks: %v", err)
	}
	if after.Size() < before.Size() {
		t.Fatalf("wal shrank from %d to %d on a healthy database", before.Size(), after.Size())
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM learnings").Scan(&n); err != nil {
		t.Fatalf("read after ticks: %v", err)
	}
	if n != 40 {
		t.Fatalf("row count after ticks = %d, want 40", n)
	}
}

// TestLos38tRelativePathAndMissingFile pins two canary/DSN edge cases the review
// found: a relative database path has to open (the DSN must carry an absolute
// path), and a canary pointed at a missing file must report it instead of
// creating an empty database.
func TestLos38tRelativePathAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	s, err := Open("yesmem.db")
	if err != nil {
		t.Fatalf("Open with a relative path: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.InsertLearning(los38tLearning(1)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := walCanary("yesmem.db"); err != nil {
		t.Fatalf("canary with a relative path: %v", err)
	}

	missing := filepath.Join(dir, "missing", "yesmem.db")
	if err := os.MkdirAll(filepath.Dir(missing), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := walCanary(missing); err == nil {
		t.Fatalf("canary reported a missing database as readable")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("canary created the missing database file (stat err = %v)", err)
	}
}

// TestLos38tGuardDoesNotCreateAMissingDatabase covers the case a review found:
// the database file itself is moved away while the process holds it. Creating an
// empty file at that path would bury the frames the moved file still holds, so
// the guard must report and leave everything alone.
func TestLos38tGuardDoesNotCreateAMissingDatabase(t *testing.T) {
	s, path := los38tOpenStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.InsertLearning(los38tLearning(i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	moved := path + ".moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("rename: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil {
			t.Fatalf("remove %s: %v", suffix, err)
		}
	}

	s.walGuardTick()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the guard recreated the database path (stat err = %v)", err)
	}
	// The held connections keep their inode, so the rows are still readable;
	// they are what a restore has to start from.
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM learnings").Scan(&n); err != nil {
		t.Fatalf("the store lost its connection: %v", err)
	}
	if n != 3 {
		t.Fatalf("row count on the held connection = %d, want 3", n)
	}
	if fi, err := os.Stat(moved); err != nil || fi.Size() == 0 {
		t.Fatalf("the moved database file was not left intact (stat err = %v)", err)
	}
}

// TestLos38tSidecarMatchIsExact pins the file set the alarm and the heal accept:
// a stale look-alike name next to the database must not count as a sidecar.
func TestLos38tSidecarMatchIsExact(t *testing.T) {
	path := "/tmp/yesmem.db"
	for _, ok := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if !isStoreFile(path, ok) {
			t.Errorf("isStoreFile(%q) = false, want true", ok)
		}
	}
	for _, other := range []string{path + ".bak", path + "-backup", path + "-wal-2026", "/tmp/other.db"} {
		if isStoreFile(path, other) {
			t.Errorf("isStoreFile(%q) = true, want false", other)
		}
	}
}

// TestLos38tGuardRefusesReadOnlyStore pins the refusal: a read-only store must
// never run a guard, because the canary checkpoints the database.
func TestLos38tGuardRefusesReadOnlyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yesmem.db")
	rw, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := rw.InsertLearning(los38tLearning(1)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", suffix, err)
		}
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { ro.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ro.StartWALGuard(ctx, 10*time.Millisecond)
	time.Sleep(150 * time.Millisecond)

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("a guard tick ran on the read-only store: %s appeared (stat err = %v)", suffix, err)
		}
	}
}

// TestLos38tDeletedSidecarFDsReportsUnlinkedFiles covers the alarm half: the scan
// must name a sidecar this process still holds after it was unlinked.
func TestLos38tDeletedSidecarFDsReportsUnlinkedFiles(t *testing.T) {
	// The sidecar scan reads /proc/<pid>/fd, which darwin does not expose.
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("the sidecar scan needs /proc/<pid>/fd")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "yesmem.db")
	wal := path + "-wal"
	f, err := os.Create(wal)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.WriteString("x"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Remove(wal); err != nil {
		t.Fatalf("remove: %v", err)
	}
	defer f.Close()

	got := deletedSidecarFDs([]string{path})
	if len(got) == 0 {
		t.Fatalf("deletedSidecarFDs reported nothing while an unlinked sidecar is held open")
	}
	found := false
	for _, g := range got {
		if g == wal {
			found = true
		}
	}
	if !found {
		t.Fatalf("deletedSidecarFDs = %v, want it to contain %q", got, wal)
	}
	if other := deletedSidecarFDs([]string{filepath.Join(dir, "unrelated.db")}); len(other) != 0 {
		t.Fatalf("deletedSidecarFDs matched an unrelated database: %v", other)
	}
}

// TestLos38tIsIOError pins the error classification. The genuine driver error is
// exercised by the canary tests; the text form and the negative cases are pinned
// here so a driver change cannot widen the gate.
func TestLos38tIsIOError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"short read", errors.New("disk I/O error (522)"), true},
		{"wrapped short read", fmt.Errorf("query: %w", errors.New("disk I/O error (522)")), true},
		{"read error", errors.New("disk I/O error (1034)"), true},
		{"other", errors.New("no such table"), false},
		{"sql err no rows", sql.ErrNoRows, false},
	}
	for _, c := range cases {
		if got := isIOError(c.err); got != c.want {
			t.Errorf("%s: isIOError = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestLos38tJournalModeIsReadableAndWAL pins the probe the guard uses to name an
// external journal mode switch.
func TestLos38tJournalModeIsReadableAndWAL(t *testing.T) {
	s, _ := los38tOpenStore(t)
	mode, err := journalModeOf(s.db)
	if err != nil {
		t.Fatalf("journalModeOf: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode = %q, want \"wal\"", mode)
	}
}

// TestLos38tGuardRunsOnInterval checks the ticker wiring: the guard heals on its
// own clock and stops with the context.
func TestLos38tGuardRunsOnInterval(t *testing.T) {
	s, path := los38tOpenStore(t)
	if _, err := s.InsertLearning(los38tLearning(1)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.StartWALGuard(ctx, 20*time.Millisecond)
	t.Cleanup(cancel)

	los38tPrepare(t, s, path, 5)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if walCanary(path) == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := walCanary(path); err != nil {
		t.Fatalf("guard did not heal within the deadline: %v", err)
	}
}

// los38tPragmaInt reads an integer pragma through the given pool.
func los38tPragmaInt(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

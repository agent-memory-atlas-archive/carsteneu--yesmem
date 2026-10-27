package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

// Store wraps an SQLite database connection.
type Store struct {
	db        *sql.DB
	runtimeDB *sql.DB
	readDB    *sql.DB

	messagesDB     *sql.DB
	messagesReadDB *sql.DB
	capStoreDB     *sql.DB
	readOnlyDB     *sql.DB

	// Paths of the files behind the pools, for the WAL guard. Empty for an
	// in-memory store.
	dbPath       string
	runtimePath  string
	messagesPath string

	// readOnlyStore marks a handle opened by OpenReadOnly, whose contract is
	// that it writes nothing. The WAL guard refuses to run there.
	readOnlyStore bool

	// walGuardStop is closed by Close to end the guard goroutine, walGuardDone
	// when that goroutine has returned. Without the wait a heal could run while
	// the pools close and recreate the database files, which is how a temp dir
	// cleanup failed with "directory not empty" (YM-391).
	walGuardMu   sync.Mutex
	walGuardStop chan struct{}
	walGuardDone chan struct{}

	ftsLastSyncID atomic.Int64 // tracks highest learning ID synced to FTS5
}

// Open creates or opens an SQLite database at the given path.
// Use ":memory:" for an in-memory database (testing).
func Open(path string) (*Store, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}

	s := &Store{db: db, runtimeDB: db, readDB: db, messagesDB: db, messagesReadDB: db, readOnlyDB: db}

	if path != ":memory:" {
		// The WAL guard matches /proc/self/fd targets against these paths, and
		// readlink reports the resolved path. A store opened through a symlinked
		// directory would never match without this.
		s.dbPath = resolveStorePath(path)
		runtimePath := filepath.Join(filepath.Dir(path), "runtime.db")
		runtimeDB, err := openSQLite(runtimePath)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("open runtime db: %w", err)
		}
		s.runtimeDB = runtimeDB
		s.runtimePath = resolveStorePath(runtimePath)

		messagesPath := filepath.Join(filepath.Dir(path), "messages.db")
		messagesDB, err := openSQLite(messagesPath)
		if err != nil {
			runtimeDB.Close()
			db.Close()
			return nil, fmt.Errorf("open messages db: %w", err)
		}
		s.messagesDB = messagesDB
		s.messagesPath = resolveStorePath(messagesPath)
	}

	if err := s.createSchema(); err != nil {
		s.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	if err := s.createRuntimeSchema(); err != nil {
		s.Close()
		return nil, fmt.Errorf("create runtime schema: %w", err)
	}

	if err := s.createMessagesSchema(); err != nil {
		s.Close()
		return nil, fmt.Errorf("create messages schema: %w", err)
	}

	if path != ":memory:" {
		readDB, err := openSQLite(path)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("open read db: %w", err)
		}
		readDB.SetMaxOpenConns(8) // WAL mode allows concurrent reads
		s.readDB = readDB

		messagesReadDB, err := openSQLite(filepath.Join(filepath.Dir(path), "messages.db"))
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("open messages read db: %w", err)
		}
		messagesReadDB.SetMaxOpenConns(8)
		s.messagesReadDB = messagesReadDB

		readOnlyDB, err := openSQLiteReadOnly(path)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("open read-only db: %w", err)
		}
		s.readOnlyDB = readOnlyDB
	}

	if err := s.migrateProxyStateToRuntime(); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate proxy state: %w", err)
	}

	return s, nil
}

// OpenReadOnly opens an existing yesmem database for reading only. Unlike Open it
// writes nothing: no schema creation, no migrations, no runtime/messages siblings
// next to the file. Reporting commands use it so they cannot mutate a live DB.
func OpenReadOnly(path string) (*Store, error) {
	db, err := openSQLiteReadOnly(path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, runtimeDB: db, readDB: db, messagesDB: db, messagesReadDB: db, readOnlyDB: db, readOnlyStore: true}
	if path != ":memory:" {
		s.dbPath = resolveStorePath(path)
	}
	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	s.stopWALGuard()
	s.CloseCapsDB()
	if s.readOnlyDB != nil && s.readOnlyDB != s.db && s.readOnlyDB != s.readDB {
		s.readOnlyDB.Close()
	}
	if s.messagesReadDB != nil && s.messagesReadDB != s.db && s.messagesReadDB != s.messagesDB {
		s.messagesReadDB.Close()
	}
	if s.messagesDB != nil && s.messagesDB != s.db {
		s.messagesDB.Close()
	}
	if s.runtimeDB != nil && s.runtimeDB != s.db {
		if err := s.runtimeDB.Close(); err != nil {
			s.db.Close()
			return err
		}
	}
	if s.readDB != nil && s.readDB != s.db && s.readDB != s.runtimeDB {
		if err := s.readDB.Close(); err != nil {
			s.db.Close()
			return err
		}
	}
	return s.db.Close()
}

// DB returns the underlying sql.DB for advanced queries.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) proxyStateDB() *sql.DB {
	if s.runtimeDB != nil {
		return s.runtimeDB
	}
	return s.db
}

func (s *Store) readerDB() *sql.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

// ReaderDB returns the read-optimized DB connection (exported for daemon use).
func (s *Store) ReaderDB() *sql.DB {
	return s.readerDB()
}

func (s *Store) messagesWriteDB() *sql.DB {
	if s.messagesDB != nil {
		return s.messagesDB
	}
	return s.db
}

func (s *Store) messagesReaderDB() *sql.DB {
	if s.messagesReadDB != nil {
		return s.messagesReadDB
	}
	return s.messagesWriteDB()
}

// MessagesDB returns the messages database connection (for direct queries).
func (s *Store) MessagesDB() *sql.DB {
	return s.messagesReaderDB()
}

// pragmaSetting is one per-connection PRAGMA, in the two forms the code needs:
// the driver's DSN parameter and the SQL statement for a connection the DSN
// cannot configure.
type pragmaSetting struct {
	name  string
	value string
}

func (p pragmaSetting) dsn() string { return p.name + "(" + p.value + ")" }
func (p pragmaSetting) sql() string { return "PRAGMA " + p.name + "=" + p.value }

// writerPragmas are the per-connection settings of a read-write database. They
// travel in the DSN because a pool that loses its connections (the WAL guard
// drops them, YM-352) would otherwise hand out connections without them: no
// busy wait, no WAL size cap, a 2MB page cache.
var writerPragmas = []pragmaSetting{
	{"busy_timeout", "30000"},
	{"synchronous", "NORMAL"},
	// Cap WAL physical size. The cap belongs to the connection that restarts the
	// log: after a fully checkpointed WAL, the next write on a connection with
	// this setting truncates the file to the limit (measured 19.10.2026: a 31MB
	// WAL, checkpoint(RESTART) left it at 31MB, the next small write cut it to
	// exactly 10MiB). A foreign writer without the setting does not cap at all.
	// Without the cap the WAL is recycled in place and stays at its peak size,
	// slowing every external sqlite3-reader by ~8s per open once it reaches
	// hundreds of MB. The truncation needs a log restart, and a restart needs
	// every read mark at the end of the log, so it cannot cut frames another
	// connection still reads (walguards_los38u_test.go pins both halves, YM-355).
	{"journal_size_limit", "10485760"},
	// Raise the page cache from the 2MB default: critical for the 55-column
	// learnings table and frequent multi-join queries (briefing, search,
	// clustering). This is a per-connection cap, so the read pools with eight
	// connections each multiply it; SQLite fills it from the pages a connection
	// actually touches, so the cap bounds memory instead of reserving it.
	{"cache_size", "-64000"},
	// Memory-mapped I/O for read-heavy workloads: proxy, briefing, hybrid search
	// (measured 19.10.2026 on a 100MB database with a warm page cache, five
	// alternating rounds, medians: 20k point lookups 1044ms against 1141ms, one
	// full scan 144ms against 270ms; the spread between rounds is wide).
	// 256MB of address space per connection, touched pages only.
	//
	// A mapped read has no recoverable error path: SQLite's xFetch cannot report
	// one, so a page the kernel will not fault in becomes a signal and kills the
	// process (sqlite.org/mmap.html). Measured 19.10.2026: a raw truncation of
	// the database file (os.Truncate) ended in SIGBUS inside _btreeInitPage; the
	// same read survives with mmap_size=0. A consistent shrink does not reach
	// that state (VACUUM plus checkpoint stayed safe in 60 rounds, and the
	// truncation needs a log restart that an open read mark blocks), and the WAL
	// incidents that do occur cost no process death because the log is not
	// mapped; they still surface as SQLITE_IOERR_SHORT_READ for new connections
	// until the WAL guard heals them (YM-352). What stays unsafe is a foreign
	// write that replaces the file below the mapping: a restore with cp or rsync
	// over the live inode has to stop the processes first.
	{"mmap_size", "268435456"},
	// Keep temporary tables (ORDER BY on unindexed columns) in memory.
	{"temp_store", "MEMORY"},
}

// readerPragmas are the per-connection settings of a read-only database.
// query_only is the one that matters most: it dies with its connection, so a
// pool that replaces connections would otherwise expose writes through a
// read-only handle.
var readerPragmas = []pragmaSetting{
	{"query_only", "1"},
	{"busy_timeout", "30000"},
}

// dsnMode is the access mode a SQLite connection is opened with. rw is explicit
// so an open can never create a database where one disappeared.
type dsnMode string

const (
	dsnReadWrite dsnMode = "rw"
	dsnReadOnly  dsnMode = "ro"
)

// sqliteDSN builds a SQLite DSN for a file path. pragmas are applied by the
// driver on every connection it opens. The path is escaped rather than
// concatenated: modernc.org/sqlite splits a DSN at the first '?' and truncates
// the path there, so a database below a directory named with '?' or '#' would
// otherwise open a different file, and in read-only mode it would silently lose
// mode=ro too. A name that is not a file path (":memory:", an existing "file:"
// URI) is returned untouched.
func sqliteDSN(path string, mode dsnMode, pragmas []pragmaSetting) string {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return path
	}
	// url.URL writes an empty host for a relative path as "file://name", and
	// SQLite reads "name" as the host, so the DSN has to carry an absolute path.
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Set("mode", string(mode))
	if mode == dsnReadOnly {
		q.Set("immutable", "0")
	}
	for _, p := range pragmas {
		q.Add("_pragma", p.dsn())
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// applyPragmas sets the pragmas on the connection a pool opens first. It is for
// the names sqliteDSN passes through untouched, where the driver ignores query
// parameters; a pool with a single long-lived connection needs nothing more.
func applyPragmas(db *sql.DB, pragmas []pragmaSetting) error {
	for _, p := range pragmas {
		if _, err := db.Exec(p.sql()); err != nil {
			return fmt.Errorf("%s: %w", p.sql(), err)
		}
	}
	return nil
}

func openSQLite(path string) (*sql.DB, error) {
	// Create the file owner-only before SQLite sees it: the -wal and -shm
	// siblings inherit the mode of the database file when they are created, and
	// a chmod on the database afterwards does not reach a WAL that already
	// exists, which is where the most recent commits sit (audit 16.10.2026,
	// YM-328).
	if err := hardenDBFile(path); err != nil {
		return nil, err
	}

	dsn := sqliteDSN(path, dsnReadWrite, writerPragmas)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	db.SetMaxOpenConns(1)

	if dsn == path {
		if err := applyPragmas(db, writerPragmas); err != nil {
			db.Close()
			return nil, err
		}
	}

	// journal_mode stays a statement of its own: it is a property of the file,
	// and switching a live database into WAL is the operation that deletes the
	// sidecars of another connection (measured 18.10.2026, YM-352).
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	// journal_mode=WAL created the siblings while the database file still had its
	// old mode; harden them as well.
	hardenDBCompanions(path)

	return db, nil
}

// resolveStorePath returns the path a file descriptor would report for this file,
// so the WAL guard can match the two. EvalSymlinks covers a data directory that
// is a symlink; Abs covers a relative one; the input is the fallback.
func resolveStorePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// dbCompanions lists the database file and the files SQLite keeps next to it.
func dbCompanions(path string) []string {
	return []string{path, path + "-wal", path + "-shm", path + "-journal"}
}

// hardenDBFile restricts a database file and its existing siblings to the owner.
// SQLite creates the file with the process umask, so on a host with umask 022 the
// knowledge base landed as 0644 in a 0755 directory and every local account could
// read it (audit 16.10.2026, YM-328). Every client runs as the same user, so
// owner-only costs no reachability. A mode that cannot be set is reported, not
// fatal: see hardenDBCompanions (YM-338).
func hardenDBFile(path string) error {
	if skipDBHarden(path) {
		return nil
	}
	// Never open an existing database file here. Closing any descriptor of a
	// file drops every POSIX lock this process holds on it, and SQLite keeps
	// believing it holds them; Open opens yesmem.db and messages.db twice, so the
	// second call stripped the first connection's lock and another process then
	// took itself for the last connection and deleted -wal and -shm under the
	// running daemon (live 18.10.2026, disk I/O error 522). A missing file is
	// created owner-only; nothing in this process can hold it open yet.
	switch _, err := os.Lstat(path); {
	case os.IsNotExist(err):
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil && !os.IsExist(err) {
			return fmt.Errorf("create %s: %w", path, err)
		}
		if err == nil {
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", path, err)
			}
		}
	case err != nil:
		return fmt.Errorf("stat %s: %w", path, err)
	}
	hardenDBCompanions(path)
	return nil
}

// hardenDBCompanions chmods the database file and its siblings. A mode that
// cannot be set is reported, not fatal: filesystems without chmod support (some
// FUSE and network mounts) would otherwise make yesmem unstartable, and the data
// directory is 0700 since YM-328, so the file is still unreachable from other
// accounts (audit 16.10.2026, YM-338).
func hardenDBCompanions(path string) {
	if skipDBHarden(path) {
		return
	}
	for _, p := range dbCompanions(path) {
		if err := chmodFile(p, 0o600); err != nil && !os.IsNotExist(err) {
			log.Printf("[storage] cannot restrict %s to owner-only: %v (mode stays %s; the data directory is 0700)", p, err, effectiveMode(p))
		}
	}
}

// effectiveMode reports the mode a file actually has, for the warning after a
// refused chmod: "cannot chmod" alone does not say whether the file is readable
// by other accounts (YM-338 review).
func effectiveMode(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%04o", info.Mode().Perm())
}

// chmodFile is a seam for the tests: a chmod failure cannot be provoked on a
// filesystem that supports chmod, and the degraded path still has to be proven.
var chmodFile = os.Chmod

// HardenDBFilesInDir restricts every database file in dir to the owner,
// including the ones that never pass through openSQLite (guard_state.db,
// caps.db) and the empty legacy stubs of older installations. Sidecars
// (-wal, -shm, -journal) carry the most recent commits and are covered too.
//
// Only regular files are touched: a symlink is not followed and a directory is
// not chmodded. A missing directory is not an error; the daemon creates it
// later. The returned error joins the failures of individual files, so a single
// foreign mount point does not hide the rest of the sweep.
func HardenDBFilesInDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	hardened := 0
	var failures []error
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !isDBFileName(entry.Name()) {
			continue
		}
		p := filepath.Join(dir, entry.Name())
		if err := chmodFile(p, 0o600); err != nil {
			if os.IsNotExist(err) {
				continue // removed between ReadDir and chmod
			}
			failures = append(failures, fmt.Errorf("chmod %s: %w", p, err))
			continue
		}
		hardened++
	}
	return hardened, errors.Join(failures...)
}

// isDBFileName reports whether name is a database file, one of its sidecars, or a
// `.db.bak…` backup copy of one (`yesmem.db.bak-pre-...`, `caps.db.bak-...`): old
// backup copies in the data directory are as sensitive as the live file.
func isDBFileName(name string) bool {
	for _, suffix := range []string{".db", ".db-wal", ".db-shm", ".db-journal"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return strings.Contains(name, ".db.bak")
}

// skipDBHarden reports paths that are not files on disk.
func skipDBHarden(path string) bool {
	return path == "" || path == ":memory:" || strings.HasPrefix(path, "file:")
}

// openSQLiteReadOnly opens a read-only connection to the same database file
// used by openSQLite. The connection enforces read-only at the driver level via
// the SQLite URI mode=ro flag and PRAGMA query_only=1 as defense in depth.
// Callers should use this for query-only access paths that accept untrusted
// SQL (e.g. the db_query CLI/RPC) so the validator is not the only safeguard.
//
// journal_mode is not part of this handle and cannot be: the mode is a property
// of the file, the write path switches it to WAL, and a mode=ro connection
// reports it but cannot change it (the statement fails, the mode=ro handle
// cannot take the write lock). A read-only open therefore inherits the writer's
// mode and never repairs it.
func openSQLiteReadOnly(path string) (*sql.DB, error) {
	dsn := sqliteDSN(path, dsnReadOnly, readerPragmas)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open ro db: %w", err)
	}

	// MaxOpenConns=1 keeps the connection count predictable for bulk reads;
	// read-only callers are not on a hot path.
	db.SetMaxOpenConns(1)

	if dsn == path {
		// A name the DSN builder passes through cannot carry mode=ro, and a
		// read-only handle without it would be a naming convention only.
		db.Close()
		return nil, fmt.Errorf("read-only handle needs a file path, got %q", path)
	}

	// Query the mode back: the read-only guarantee must hold, not just be named.
	var mode string
	if err := db.QueryRow("PRAGMA query_only").Scan(&mode); err != nil {
		db.Close()
		return nil, fmt.Errorf("set query_only: %w", err)
	}
	if mode != "1" {
		db.Close()
		return nil, fmt.Errorf("read-only handle is not query_only (got %q)", mode)
	}

	return db, nil
}

// OpenSQLiteReadOnly opens a SQLite file this process does not own, with the same
// guarantees as the read-only handle of a Store: mode=ro in the DSN and
// PRAGMA query_only=1 on the connection. Foreign application databases (the
// opencode session DB) are opened through this instead of sql.Open, so a read
// path cannot write to them by accident.
func OpenSQLiteReadOnly(path string) (*sql.DB, error) {
	return openSQLiteReadOnly(path)
}

// into the FTS5 index. Replaces the old trigger-based approach which caused
// write contention blocking BM25 reads for 5-60s during extraction/evolution.
func (s *Store) StartFTSSync(ctx context.Context, interval time.Duration) {
	// Initialize last sync ID from current FTS5 state
	var maxID int64
	s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM learnings_fts`).Scan(&maxID)
	s.ftsLastSyncID.Store(maxID)
	log.Printf("[fts-sync] started (interval=%v, last_id=%d)", interval, maxID)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.syncFTS()
			}
		}
	}()
}

// syncFTS inserts new learnings into FTS5 in a single batch transaction.
func (s *Store) syncFTS() {
	lastID := s.ftsLastSyncID.Load()

	// Find new learnings since last sync
	rows, err := s.readerDB().Query(`SELECT id, content || ' ' || COALESCE(trigger_rule, '') FROM learnings WHERE id > ? ORDER BY id`, lastID)
	if err != nil {
		log.Printf("[fts-sync] read error: %v", err)
		return
	}
	defer rows.Close()

	type pending struct {
		id      int64
		content string
	}
	var items []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.content) == nil {
			items = append(items, p)
		}
	}
	if len(items) == 0 {
		return
	}

	// Batch insert into FTS5 — single transaction, single COMMIT
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("[fts-sync] begin tx: %v", err)
		return
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO learnings_fts(rowid, content) VALUES (?, ?)`)
	if err != nil {
		log.Printf("[fts-sync] prepare: %v", err)
		return
	}
	defer stmt.Close()

	for _, item := range items {
		stmt.Exec(item.id, item.content)
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[fts-sync] commit: %v", err)
		return
	}

	maxNew := items[len(items)-1].id
	s.ftsLastSyncID.Store(maxNew)
	log.Printf("[fts-sync] synced %d learnings (id %d→%d)", len(items), lastID, maxNew)
}

// SyncFTSNow runs a synchronous FTS5 sync. Intended for tests where no
// background sync goroutine is running (in-memory SQLite).
func (s *Store) SyncFTSNow() {
	s.syncFTS()
}

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// The WAL guard heals a failure an external writer can produce at any time: the
// database file stays intact, but the -wal and -shm files next to it are
// replaced, truncated or unlinked while this process holds them open. Measured
// on 18.10.2026 (YM-352): a stale wal-index next to a WAL that was cut short
// makes every NEW connection fail with SQLITE_IOERR_SHORT_READ ("disk I/O
// error (522)"), while connections opened before the tampering keep working on
// their stale file handles. That asymmetry is what makes the failure look like a
// restart problem: only a new process inherits no poisoned state.
//
// The poison is process-wide, not connection-wide. SQLite keeps one shared
// memory object per database inode (os_unix.c), so while any connection to that
// file is open, a new connection joins the stale wal-index and fails again.
//
// The heal therefore does the one thing that helps: drop every pooled connection
// of that database at once, then let a fresh connection run WAL recovery. No
// file is touched. Closing the last old connection checkpoints what its deleted
// WAL still holds into the database file, so the heal rolls nothing back.
//
// mmap_size is not involved: the reproduction fails with mmap_size=0 as well.

// idleConnections is the database/sql default idle limit. Restoring it returns a
// pool to the shape Open built: SetMaxOpenConns(1) clamps it to one.
const idleConnections = 2

// canaryTimeout bounds the canary's own busy wait, not the heal.
const canaryTimeout = 5 * time.Second

// isIOError reports whether err belongs to SQLite's SQLITE_IOERR family. The
// sub-codes (SHORT_READ 522, READ 1034, FSYNC 1038) are what a foreign writer
// produces. SQLITE_BUSY and SQLITE_CORRUPT are not IOERR, and dropping
// connections cannot repair either.
func isIOError(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()&0xff == 10 // SQLITE_IOERR
	}
	// A wrapped or text-only error still names the family.
	return strings.Contains(err.Error(), "disk I/O error")
}

// walCanary reads the write-ahead log through a connection of its own. A
// checkpoint walks the wal-index and therefore fails exactly when a stale index
// meets a log that no longer matches it; measured on 18.10.2026 for a WAL
// truncated to 0, 5, 32 and 1024 bytes, all four with the IOERR family. A plain
// schema read does not see the damage, so the canary must checkpoint.
//
// It cannot see a -shm that was removed on its own: SQLite rebuilds the index
// from an intact log. That state is the deleted-descriptor alarm's job.
func walCanary(path string) error {
	// Without this check os.Open with O_CREATE would create an empty database at
	// the path and report success, hiding a database that is gone.
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("database file: %w", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, dsnReadWrite, []pragmaSetting{{"busy_timeout", "5000"}}))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), canaryTimeout)
	defer cancel()
	// The pragma returns a row; Exec steps it to completion, which is what
	// surfaces a broken log. Its values mean nothing here, so they are dropped.
	_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
	return err
}

// journalModeOf reads the persistent journal mode of a database through the pool
// that writes it. The mode lives in the database header, so every connection
// reports the same value.
func journalModeOf(db *sql.DB) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), canaryTimeout)
	defer cancel()
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return "", err
	}
	return strings.ToLower(mode), nil
}

// dbPool is one database file with every connection pool this store holds on it.
// readOnlyDB points at the same file as db and readDB, so it pins the shared
// SQLite state just as well, and it has to be dropped with them.
type dbPool struct {
	name   string
	path   string
	writer *sql.DB
	all    []*sql.DB
}

// walGuardPools lists the pools to watch, grouped by database file. caps.db is
// not among them: it runs with journal_mode=DELETE (cap_store.go), so it has no
// WAL to poison. Its rollback journal is a different failure and a different
// heal.
func (s *Store) walGuardPools() []dbPool {
	main := dbPool{name: "yesmem.db", path: s.dbPath, writer: s.db, all: []*sql.DB{s.db}}
	if s.readDB != nil && s.readDB != s.db {
		main.all = append(main.all, s.readDB)
	}
	if s.readOnlyDB != nil && s.readOnlyDB != s.db && s.readOnlyDB != s.readDB {
		main.all = append(main.all, s.readOnlyDB)
	}
	pools := []dbPool{main}

	if s.runtimeDB != nil && s.runtimeDB != s.db {
		pools = append(pools, dbPool{name: "runtime.db", path: s.runtimePath, writer: s.runtimeDB, all: []*sql.DB{s.runtimeDB}})
	}
	if s.messagesDB != nil && s.messagesDB != s.db {
		messages := dbPool{name: "messages.db", path: s.messagesPath, writer: s.messagesDB, all: []*sql.DB{s.messagesDB}}
		if s.messagesReadDB != nil && s.messagesReadDB != s.messagesDB {
			messages.all = append(messages.all, s.messagesReadDB)
		}
		pools = append(pools, messages)
	}
	return pools
}

// walGuardPaths lists the database files the guard watches.
func (s *Store) walGuardPaths() []string {
	paths := make([]string, 0, 3)
	for _, pool := range s.walGuardPools() {
		if pool.path != "" {
			paths = append(paths, pool.path)
		}
	}
	return paths
}

// walGuardTickHook runs at the start of every tick. It is nil in production; a
// test holds one tick open to assert that Close waits for it (YM-391).
var walGuardTickHook func()

// walGuardTick is one round: name the sidecars this process still holds after
// they were unlinked, then heal any database a fresh connection cannot read.
func (s *Store) walGuardTick() {
	if walGuardTickHook != nil {
		walGuardTickHook()
	}
	pools := s.walGuardPools()

	deleted := deletedSidecarFDs(s.walGuardPaths())
	for _, target := range dedupePaths(deleted) {
		log.Printf("[wal-guard] holding a connection on a deleted file: %s", target)
	}

	for _, pool := range pools {
		if pool.path == "" || pool.writer == nil {
			continue
		}
		// A database that is gone cannot be healed, only restored. Without this
		// check the recovery connection would open with SQLITE_OPEN_CREATE,
		// create an empty database at the path and report success, which buries
		// the frames a moved-away file still holds.
		if _, err := os.Stat(pool.path); err != nil {
			log.Printf("[wal-guard] %s: the database file itself is unreachable (%v); a restore is the only way, the pools are left alone", pool.name, err)
			continue
		}
		if mode, err := journalModeOf(pool.writer); err == nil && mode != "wal" {
			// A second connection can switch the journal mode of a live database,
			// and SQLite deletes -wal and -shm when it does (measured 18.10.2026).
			// The guard does not switch back: that is the operation that destroyed
			// the sidecars. Until the next start the database stays in that mode.
			log.Printf("[wal-guard] %s: journal mode is %q instead of \"wal\", an external writer switched it; it stays that way until the next start", pool.name, mode)
		}

		canaryErr := walCanary(pool.path)
		staleHandles := poolAffectedByDeleted(pool.path, deleted)
		if !staleHandles && !isIOError(canaryErr) {
			if canaryErr != nil {
				log.Printf("[wal-guard] %s: a fresh connection cannot checkpoint: %v; repair is manual", pool.name, canaryErr)
			}
			continue
		}
		if canaryErr != nil {
			log.Printf("[wal-guard] %s: disk I/O error on a fresh connection: %v", pool.name, canaryErr)
		} else {
			log.Printf("[wal-guard] %s: pooled connections outlive their sidecars, dropping them", pool.name)
		}

		walBefore := sidecarSize(pool.path + "-wal")
		restore := dropPooledConnections(pool.all)
		err := recoverPool(pool.writer)
		restore()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				// The only writer connection was checked out; nothing was retried.
				log.Printf("[wal-guard] %s: the pool was busy, nothing to report; next tick retries", pool.name)
				continue
			}
			log.Printf("[wal-guard] %s: still failing after reconnecting: %v; next tick retries", pool.name, err)
			continue
		}
		if err := walCanary(pool.path); err != nil {
			log.Printf("[wal-guard] %s: reconnected, but a fresh connection still cannot checkpoint: %v", pool.name, err)
			continue
		}
		// The recovery created the sidecars again, and their mode comes from the
		// database file at creation time (YM-328). A mode that cannot be set is
		// reported by hardenDBCompanions itself, not treated as a failed heal
		// (YM-338).
		hardenDBCompanions(pool.path)
		log.Printf("[wal-guard] %s: healed, a fresh connection recovered the log (it was %d bytes; frames a cut log lost do not come back)", pool.name, walBefore)
	}
}

// sidecarSize reports the current size of a sidecar, or -1 when it is absent.
func sidecarSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// dedupePaths keeps the first occurrence of each path. SQLite holds more than one
// descriptor on the same sidecar, and a repeated line per tick reads like a
// second event.
func dedupePaths(paths []string) []string {
	if len(paths) < 2 {
		return paths
	}
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// isStoreFile reports whether a path is one of the files SQLite keeps for this
// database. The exact set matters: a prefix match would treat a stale
// "<db>-backup" as a sidecar and drop every pool for nothing.
func isStoreFile(path, candidate string) bool {
	for _, p := range dbCompanions(path) {
		if p == candidate {
			return true
		}
	}
	return false
}

// poolAffectedByDeleted reports whether an unlinked file belongs to this
// database file.
func poolAffectedByDeleted(path string, deleted []string) bool {
	for _, d := range deleted {
		if isStoreFile(path, d) {
			return true
		}
	}
	return false
}

// recoverPool forces the pool to open a new connection and read the database
// header through it. That read is what runs WAL recovery once the previous
// connections are gone.
func recoverPool(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), canaryTimeout)
	defer cancel()
	var version int
	return db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&version)
}

// dropPooledConnections closes every idle connection of the given pools and
// stops them from keeping new ones, so SQLite releases its per-inode state. A
// connection that is checked out right now closes when it returns. The returned
// function restores the idle limit.
func dropPooledConnections(handles []*sql.DB) func() {
	for _, db := range handles {
		db.SetMaxIdleConns(0)
	}
	return func() {
		for _, db := range handles {
			db.SetMaxIdleConns(idleConnections)
		}
	}
}

// StartWALGuard runs the heal loop until ctx is done or the store is closed. A
// read-only store is refused: its contract is that it writes nothing, and the
// canary checkpoints.
//
// Close ends the loop too and waits for it: a heal that runs after the pools
// close recreates the database files, so a caller that removes the directory
// afterwards finds it not empty (YM-391).
func (s *Store) StartWALGuard(ctx context.Context, interval time.Duration) {
	if s.readOnlyStore {
		log.Printf("[wal-guard] not starting on a read-only store: the canary checkpoints the database")
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	// A second call would otherwise overwrite the channels of the first loop and
	// leave it unsignallable: a guard that outlives Close and heals after the pools
	// are gone. One call site today, but the failure it would cause is the one this
	// change exists to close.
	s.stopWALGuard()
	log.Printf("[wal-guard] started (interval=%v)", interval)
	stop := make(chan struct{})
	done := make(chan struct{})
	s.walGuardMu.Lock()
	s.walGuardStop, s.walGuardDone = stop, done
	s.walGuardMu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				s.walGuardTick()
			}
		}
	}()
}

// stopWALGuard ends the guard goroutine and waits for a tick that is running
// right now. A second call is a no-op for the channels, so a double Close cannot
// close one twice — but the wait is per caller: a Close that runs while another
// one is already waiting proceeds without it (one deferred Close per process
// today, so that shape is not reachable).
func (s *Store) stopWALGuard() {
	s.walGuardMu.Lock()
	stop, done := s.walGuardStop, s.walGuardDone
	s.walGuardStop, s.walGuardDone = nil, nil
	s.walGuardMu.Unlock()
	if stop == nil || done == nil {
		return
	}
	close(stop)
	<-done
}

// deletedSidecarFDs names the held file descriptors whose path no longer exists,
// restricted to the given databases and their SQLite sidecars. It reads
// /proc/self/fd, so on other platforms the scan stays empty and the canary
// carries the heal on its own.
func deletedSidecarFDs(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	var hits []string
	for _, entry := range entries {
		target, err := os.Readlink("/proc/self/fd/" + entry.Name())
		if err != nil {
			continue
		}
		const deleted = " (deleted)"
		if !strings.HasSuffix(target, deleted) {
			continue
		}
		target = strings.TrimSuffix(target, deleted)
		for _, path := range paths {
			if isStoreFile(path, target) {
				hits = append(hits, target)
				break
			}
		}
	}
	return hits
}

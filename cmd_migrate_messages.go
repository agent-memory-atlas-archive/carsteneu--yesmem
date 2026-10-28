package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/carsteneu/yesmem/internal/daemon"

	"modernc.org/sqlite"
)

// runMigrateMessages moves the messages table out of the main database into
// messages.db, then drops it from the source.
//
// The run deletes every row of a live file and rewrites it, so it refuses to
// start while anything else uses that file. --force is the deliberate exception
// (audit 16.10.2026, YM-360).
func runMigrateMessages() {
	if err := migrateMessages(yesmemDataDir(), hasFlag(os.Args[2:], "--force"), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-messages: %v\n", err)
		os.Exit(1)
	}
}

func migrateMessages(dataDir string, force bool, out io.Writer) error {
	mainDB := filepath.Join(dataDir, "yesmem.db")
	messagesDB := filepath.Join(dataDir, "messages.db")

	fmt.Fprintf(out, "Migrating messages from %s to %s\n", mainDB, messagesDB)

	// Nothing to move on an installation that was never created, and no reason to
	// create the file this run would otherwise open.
	if _, err := os.Stat(mainDB); os.IsNotExist(err) {
		fmt.Fprintf(out, "No database at %s — nothing to migrate.\n", mainDB)
		return nil
	}

	if force {
		// Rows the daemon writes between the copy and the DELETE are deleted
		// without ever having been copied.
		fmt.Fprintln(out, "warning: --force skips the daemon and lock check; messages written during the run are lost")
	}
	if err := requireExclusiveAccess(dataDir, []string{mainDB, messagesDB}, force); err != nil {
		return err
	}

	// Open source DB
	src, err := sql.Open("sqlite", mainDB)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=10000"} {
		if _, err := src.Exec(pragma); err != nil {
			return fmt.Errorf("%s: %w", pragma, err)
		}
	}

	// Check if messages exist in source
	var count int64
	if err := src.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
		return fmt.Errorf("count source messages: %w", err)
	}
	if count == 0 {
		// A completed run can still be interrupted after the delete. There is
		// nothing left to copy then, but a leftover index must not survive behind a
		// successful exit.
		if err := finishLeftoverCleanup(dataDir, messagesDB, force, out); err != nil {
			return err
		}
		fmt.Fprintln(out, "No messages in source DB — nothing to migrate.")
		return nil
	}
	fmt.Fprintf(out, "Found %d messages to migrate\n", count)

	// Open/create target DB
	dst, err := sql.Open("sqlite", messagesDB)
	if err != nil {
		return fmt.Errorf("open target: %w", err)
	}
	defer dst.Close()
	dst.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=10000", "PRAGMA synchronous=NORMAL"} {
		if _, err := dst.Exec(pragma); err != nil {
			return fmt.Errorf("%s (target): %w", pragma, err)
		}
	}

	// Create schema in target. Must match the canonical messages table in
	// internal/storage/schema.go: a column missing here is dropped from every
	// copied row.
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS messages (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id      TEXT NOT NULL,
		source_agent    TEXT DEFAULT 'claude',
		role            TEXT NOT NULL,
		message_type    TEXT NOT NULL,
		content         TEXT,
		content_blob    BLOB,
		tool_name       TEXT,
		file_path       TEXT,
		timestamp       TEXT NOT NULL,
		sequence        INTEGER NOT NULL,
		model           TEXT DEFAULT ''
	)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_type ON messages(message_type)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_file ON messages(file_path) WHERE file_path IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_messages_session_type ON messages(session_id, message_type, sequence)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(content, tokenize='unicode61', content_rowid=id)`,
	} {
		if _, err := dst.Exec(stmt); err != nil {
			return fmt.Errorf("create target schema: %w", err)
		}
	}
	// The pair index the store maintains (storage.messageSequenceIndex). The resume
	// path compares source and target rows on exactly this pair, so without it the
	// anti-join scans every row of the session per source row.
	if err := ensureUniqueSequenceIndex(dst, messagesDB); err != nil {
		return err
	}
	// Same contract as the main database (storage.hardenDBCompanions, YM-328):
	// messages.db holds every message body and is owner-only, sidecars included.
	// The sidecars are created by the WAL pragma above and keep the umask mode.
	for _, p := range []string{messagesDB, messagesDB + "-wal", messagesDB + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("harden target: %w", err)
		}
	}

	// Check if already migrated (idempotent)
	var existing int64
	if err := dst.QueryRow("SELECT COUNT(*) FROM messages").Scan(&existing); err != nil {
		return fmt.Errorf("count target messages: %w", err)
	}

	startTime := time.Now()
	copied := int64(0)
	if existing > 0 {
		// Nothing is copied in this branch, so the target has to hold every source
		// row with the same body before any of them is deleted.
		if existing < count {
			return fmt.Errorf("target holds %d of %d messages: an earlier run was interrupted before it copied every row, move %s aside and retry", existing, count, messagesDB)
		}
		missing, err := sourceRowsMissingFromTarget(src, messagesDB)
		if err != nil {
			return fmt.Errorf("compare target with source: %w", err)
		}
		if missing > 0 {
			// Reporting success here would hide an unfinished migration: the source
			// still holds the only copy of those rows.
			return fmt.Errorf("target holds %d messages but %d source messages are missing from it or differ: an earlier run was interrupted or the file belongs to another installation. Move %s aside and retry; deleting it would destroy every message that only it holds", existing, missing, messagesDB)
		}
		// Every source row is already there: an earlier run copied them and stopped
		// before the DELETE. Recopying would duplicate rows, so this run finishes
		// the remaining steps instead of reporting "nothing to do".
		fmt.Fprintf(out, "Target already holds all %d source messages, completing the interrupted run.\n", count)
		copied = count
	} else {
		// The unique pair index rejects a source that still carries duplicates; the
		// store purges them (keeping the newest row per pair) on every Open, so the
		// installed binary is the tool that repairs this, not the migration.
		dupes, err := countDuplicateSequencePairs(src)
		if err != nil {
			return fmt.Errorf("check source duplicates: %w", err)
		}
		if dupes > 0 {
			return fmt.Errorf("source holds %d duplicate (session_id, sequence) pairs: open the installation once with a current binary (it keeps the newest row per pair) and retry", dupes)
		}
		copied, err = copyMessagesToTarget(src, dst, out, count)
		if err != nil {
			return err
		}
	}

	// A source that changed during the paging (a hook, a --force run against a live
	// daemon) can leave rows uncopied. Deleting them would drop them from both
	// files, so the count has to match what the run started with.
	if copied != count {
		return fmt.Errorf("copied %d of %d messages: the source changed during the run, nothing was deleted; delete %s and retry", copied, count, messagesDB)
	}

	fmt.Fprintf(out, "Migration complete: %d messages in %.1fs\n", copied, time.Since(startTime).Seconds())

	// The copied rows must be durable before the source rows disappear: the target
	// runs with synchronous=NORMAL, so committing alone leaves them in its WAL.
	if err := checkpointTruncate(dst); err != nil {
		return fmt.Errorf("checkpoint target: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close target: %w", err)
	}

	// Delete messages from source DB. Re-check first: the copy can take minutes,
	// and a daemon or a hook that started in the meantime must not have its rows
	// deleted behind its back.
	if err := requireExclusiveAccess(dataDir, []string{mainDB, messagesDB}, force); err != nil {
		return fmt.Errorf("before delete: %w", err)
	}
	fmt.Fprint(out, "Deleting messages from source DB... ")
	// Qualified: the coverage check attaches messages.db as target_check, and an
	// unqualified name in a multi-schema connection is a needless ambiguity for a
	// destructive statement.
	if _, err := src.Exec("DELETE FROM main.messages"); err != nil {
		return fmt.Errorf("delete from source: %w", err)
	}
	fmt.Fprintln(out, "done.")

	// Close before the swap: the compacted file replaces the one this handle holds.
	if err := src.Close(); err != nil {
		return fmt.Errorf("close source: %w", err)
	}

	// VACUUM rewrites the live file in place and cannot be rolled back. Build the
	// compacted copy beside it and rename that copy into place instead.
	fmt.Fprint(out, "Compacting source DB... ")
	if err := compactDatabase(mainDB); err != nil {
		return fmt.Errorf("compact source: %w", err)
	}
	fmt.Fprintln(out, "done.")

	// Remove Bleve index if it exists. Only now: until the swap above, the FTS
	// table is not a complete replacement for it.
	bleveDir := filepath.Join(dataDir, "bleve-index")
	bleveInfo, statErr := os.Stat(bleveDir)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("stat bleve index: %w", statErr)
	}
	if statErr == nil && bleveInfo.IsDir() {
		// Compacting a 10 GB file takes minutes; a daemon that started in between
		// must not lose the index under itself.
		if err := requireExclusiveAccess(dataDir, []string{mainDB, messagesDB}, force); err != nil {
			return fmt.Errorf("before removing the bleve index: %w", err)
		}
		fmt.Fprintf(out, "Removing Bleve index (%s)... ", bleveDir)
		if err := os.RemoveAll(bleveDir); err != nil {
			return fmt.Errorf("remove bleve index: %w", err)
		}
		fmt.Fprintln(out, "done.")
	}

	// Report sizes
	srcInfo, _ := os.Stat(mainDB)
	dstInfo, _ := os.Stat(messagesDB)
	if srcInfo != nil && dstInfo != nil {
		fmt.Fprintf(out, "\nResult:\n  %s: %d MB\n  %s: %d MB\n",
			filepath.Base(mainDB), srcInfo.Size()/(1024*1024),
			filepath.Base(messagesDB), dstInfo.Size()/(1024*1024))
	}
	return nil
}

// copyMessagesToTarget copies the source rows batch by batch and builds the FTS
// index from what it wrote. It pages on the ascending id (keyset) rather than
// LIMIT/OFFSET: an offset scan re-reads every row it has already passed, which
// makes a long migration quadratic in the number of rows.
func copyMessagesToTarget(src, dst *sql.DB, out io.Writer, count int64) (int64, error) {
	start := time.Now()
	const batchSize = 1000
	lastID := int64(0)
	copied := int64(0)

	for {
		rows, err := src.Query(`SELECT id, session_id, source_agent, role, message_type, content, content_blob, tool_name, file_path, timestamp, sequence, model FROM messages WHERE id > ? ORDER BY id LIMIT ?`, lastID, batchSize)
		if err != nil {
			return copied, fmt.Errorf("query source: %w", err)
		}

		tx, err := dst.Begin()
		if err != nil {
			rows.Close()
			return copied, fmt.Errorf("begin tx: %w", err)
		}

		stmt, err := tx.Prepare(`INSERT INTO messages (session_id, source_agent, role, message_type, content, content_blob, tool_name, file_path, timestamp, sequence, model) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			rows.Close()
			tx.Rollback()
			return copied, fmt.Errorf("prepare insert: %w", err)
		}
		ftsStmt, err := tx.Prepare(`INSERT INTO messages_fts(rowid, content) VALUES (?, ?)`)
		if err != nil {
			rows.Close()
			stmt.Close()
			tx.Rollback()
			return copied, fmt.Errorf("prepare fts insert: %w", err)
		}

		batchCount := 0
		for rows.Next() {
			var id int64
			var sessionID, role, msgType, timestamp string
			var content sql.NullString
			var contentBlob []byte
			var toolName, filePath sql.NullString
			var sourceAgent, model sql.NullString
			var sequence int

			if err := rows.Scan(&id, &sessionID, &sourceAgent, &role, &msgType, &content, &contentBlob, &toolName, &filePath, &timestamp, &sequence, &model); err != nil {
				rows.Close()
				stmt.Close()
				ftsStmt.Close()
				tx.Rollback()
				return copied, fmt.Errorf("scan: %w", err)
			}
			lastID = id

			// The write path (storage.insertMessagesTx) puts a thinking blob's text
			// into content as well, so search snippets and keyword scans see it. A
			// migrated row has to look the same.
			stored := content
			if (!content.Valid || content.String == "") && len(contentBlob) > 0 {
				stored = sql.NullString{String: string(contentBlob), Valid: true}
			}

			res, err := stmt.Exec(sessionID, sourceAgent, role, msgType, stored, contentBlob, toolName, filePath, timestamp, sequence, model)
			if err != nil {
				rows.Close()
				stmt.Close()
				ftsStmt.Close()
				tx.Rollback()
				return copied, fmt.Errorf("insert: %w", err)
			}

			// stored already carries a thinking blob's text, so the FTS row matches
			// what the write path would have indexed.
			fts := stored.String
			if fts != "" {
				id, err := res.LastInsertId()
				if err != nil {
					rows.Close()
					stmt.Close()
					ftsStmt.Close()
					tx.Rollback()
					return copied, fmt.Errorf("last insert id: %w", err)
				}
				// An incomplete FTS table is an incomplete rebuild: the bleve index
				// may only go once every row is in it.
				if _, err := ftsStmt.Exec(id, fts); err != nil {
					rows.Close()
					stmt.Close()
					ftsStmt.Close()
					tx.Rollback()
					return copied, fmt.Errorf("insert fts: %w", err)
				}
			}

			batchCount++
			copied++
		}
		rows.Close()
		stmt.Close()
		ftsStmt.Close()
		if err := rows.Err(); err != nil {
			tx.Rollback()
			return copied, fmt.Errorf("read source rows: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return copied, fmt.Errorf("commit: %w", err)
		}

		if copied%10000 == 0 || batchCount < batchSize {
			fmt.Fprintf(out, "  %d / %d messages copied (%.1fs)\n", copied, count, time.Since(start).Seconds())
		}

		if batchCount < batchSize {
			break
		}
	}
	return copied, nil
}

// sourceRowsMissingFromTarget counts source messages that have no counterpart in
// the target: same (session_id, sequence), same effective content, same metadata.
// Both files are SQLite databases, so the target is attached and compared in one
// statement instead of paging tuples through Go. Neither a count nor the identity
// pair alone is enough: a target of the same size can hold a different set of rows,
// and the same pair can carry a different body or different attribution, which
// would make the source's version the only copy of it. Effective content treats a
// blob-only row and its mirrored copy as equal, which is what the copy writes.
func sourceRowsMissingFromTarget(src *sql.DB, messagesDB string) (int64, error) {
	// The path is bound, not interpolated. SQLite still decides URI-vs-path from the
	// string value itself, so a "file:" prefix would retarget the attach; the value
	// comes from os.UserHomeDir via dataDir, not from input. The comparison runs on
	// the connection this handle owns, because src is limited to one connection.
	if _, err := src.Exec("ATTACH DATABASE ? AS target_check", messagesDB); err != nil {
		return 0, fmt.Errorf("attach %s: %w", messagesDB, err)
	}
	defer src.Exec("DETACH DATABASE target_check")

	var missing int64
	if err := src.QueryRow(`SELECT COUNT(*) FROM messages m WHERE NOT EXISTS (
		SELECT 1 FROM target_check.messages t
		WHERE t.session_id = m.session_id AND t.sequence = m.sequence
		  AND COALESCE(NULLIF(t.content, ''), CAST(t.content_blob AS TEXT), '')
		      = COALESCE(NULLIF(m.content, ''), CAST(m.content_blob AS TEXT), '')
		  AND t.role IS m.role
		  AND t.message_type IS m.message_type
		  AND t.source_agent IS m.source_agent
		  AND t.tool_name IS m.tool_name
		  AND t.file_path IS m.file_path
		  AND t.timestamp IS m.timestamp
		  AND t.model IS m.model)`).Scan(&missing); err != nil {
		return 0, fmt.Errorf("compare rows: %w", err)
	}
	return missing, nil
}

// ensureUniqueSequenceIndex makes the target's (session_id, sequence) index UNIQUE.
// CREATE UNIQUE INDEX IF NOT EXISTS is a silent no-op when a non-unique index of
// that name already exists (storage.ensureUniqueMessageSequence guards the same
// trap), and a non-unique index leaves the coverage check scanning a whole session
// per source row.
func ensureUniqueSequenceIndex(db *sql.DB, messagesDB string) error {
	const name = "idx_messages_session_seq"
	exists, unique, err := sequenceIndexState(db, name)
	if err != nil {
		return fmt.Errorf("read target indexes: %w", err)
	}
	if exists && unique {
		return nil
	}
	if exists {
		if _, err := db.Exec("DROP INDEX " + name); err != nil {
			return fmt.Errorf("drop non-unique %s: %w", name, err)
		}
	}
	if _, err := db.Exec("CREATE UNIQUE INDEX " + name + " ON messages(session_id, sequence)"); err != nil {
		return fmt.Errorf("create unique index on (session_id, sequence): %w. The target holds duplicate pairs; open the installation once with a current binary (it keeps the newest row per pair) and retry, or move %s aside", err, messagesDB)
	}
	return nil
}

// sequenceIndexState reports whether the named index exists on messages and whether
// it is unique.
func sequenceIndexState(db *sql.DB, name string) (exists, unique bool, err error) {
	rows, err := db.Query(`PRAGMA index_list(messages)`)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq, isUnique int
		var indexName, origin string
		var partial int
		if err := rows.Scan(&seq, &indexName, &isUnique, &origin, &partial); err != nil {
			return false, false, err
		}
		if indexName == name {
			return true, isUnique == 1, nil
		}
	}
	return false, false, rows.Err()
}

// countDuplicateSequencePairs reports how many (session_id, sequence) groups the
// source holds more than once.
func countDuplicateSequencePairs(src *sql.DB) (int64, error) {
	var dupes int64
	err := src.QueryRow(`SELECT COUNT(*) FROM (
		SELECT session_id, sequence FROM messages
		GROUP BY session_id, sequence HAVING COUNT(*) > 1)`).Scan(&dupes)
	if err != nil {
		return 0, err
	}
	return dupes, nil
}

// finishLeftoverCleanup removes the bleve index of a migration that was interrupted
// after its delete. Without it the index survives every retry behind an exit 0: the
// retry finds no source rows and returns before the removal step.
func finishLeftoverCleanup(dataDir, messagesDB string, force bool, out io.Writer) error {
	bleveDir := filepath.Join(dataDir, "bleve-index")
	info, err := os.Stat(bleveDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat bleve index: %w", err)
	}
	if !info.IsDir() {
		return nil
	}
	// Only an installation whose messages already live in messages.db has a finished
	// migration to clean up after. Asking the file, not sql.Open, keeps a run without
	// a target from creating one.
	if _, err := os.Stat(messagesDB); err != nil {
		return nil
	}
	target, err := sql.Open("sqlite", messagesDB)
	if err != nil {
		return fmt.Errorf("open target: %w", err)
	}
	defer target.Close()
	var targetRows int64
	if err := target.QueryRow("SELECT COUNT(*) FROM messages").Scan(&targetRows); err != nil || targetRows == 0 {
		return nil
	}
	if err := requireExclusiveAccess(dataDir, []string{filepath.Join(dataDir, "yesmem.db"), messagesDB}, force); err != nil {
		return fmt.Errorf("before removing the bleve index: %w", err)
	}
	fmt.Fprintf(out, "Removing leftover Bleve index (%s)... ", bleveDir)
	if err := os.RemoveAll(bleveDir); err != nil {
		return fmt.Errorf("remove bleve index: %w", err)
	}
	fmt.Fprintln(out, "done.")
	return nil
}

// requireExclusiveAccess refuses to touch an installation that something else is
// using. Without it the run deletes rows behind a running daemon's back.
func requireExclusiveAccess(dataDir string, paths []string, force bool) error {
	if force {
		return nil
	}
	// Not daemon.IsDaemonRunning: that follows YESMEM_SOCK, so a stale value would
	// silently disable the one guard this command depends on.
	if daemonStillAnswers(daemon.SocketPath(dataDir)) {
		return fmt.Errorf("a daemon answers on %s: stop it first, or pass --force to migrate anyway", daemon.SocketPath(dataDir))
	}
	for _, path := range paths {
		held, err := dbWriteLockHeld(path)
		if err != nil {
			return fmt.Errorf("check lock on %s: %w", path, err)
		}
		if held {
			return fmt.Errorf("%s is locked by another process: close it first, or pass --force to migrate anyway", path)
		}
	}
	return nil
}

// dbWriteLockHeld reports whether another connection currently holds a write
// lock on path. A missing file is not locked.
func dbWriteLockHeld(path string) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Zero means the check fails instead of waiting out someone else's write.
	if _, err := db.Exec("PRAGMA busy_timeout=0"); err != nil {
		return false, err
	}
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		if isBusyOrLocked(err) {
			return true, nil
		}
		return false, err
	}
	if _, err := db.Exec("ROLLBACK"); err != nil {
		return false, err
	}
	return false, nil
}

func isBusyOrLocked(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
		return true
	}
	return false
}

// checkpointTruncate moves every WAL frame into the main file and resets the log.
// A PRAGMA reports an incomplete checkpoint in the result row, not as an error, so
// discarding the row would throw away frames that never reached the main file.
func checkpointTruncate(db *sql.DB) error {
	var busy, logFrames, checkpointed int
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 || checkpointed != logFrames {
		return fmt.Errorf("wal_checkpoint(TRUNCATE) incomplete: busy=%d, %d of %d frames checkpointed", busy, checkpointed, logFrames)
	}
	return nil
}

// compactDatabase writes a vacuumed copy of path next to it and renames that copy
// over the original. The source stays intact until the rename, which is the whole
// point: an in-place VACUUM that fails leaves a half compacted database.
func compactDatabase(path string) error {
	tmp := path + ".vacuum"
	// VACUUM INTO refuses to write to an existing file.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmp)
		}
	}()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=10000"); err != nil {
		db.Close()
		return fmt.Errorf("busy_timeout: %w", err)
	}
	if err := checkpointTruncate(db); err != nil {
		db.Close()
		return err
	}
	if _, err := db.Exec("VACUUM INTO ?", tmp); err != nil {
		db.Close()
		return fmt.Errorf("VACUUM INTO: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	// VACUUM INTO creates the copy under the process umask; the swap must not widen
	// the mode of a database that was hardened to 0600.
	info, statErr := os.Stat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	if statErr == nil {
		if err := os.Chmod(tmp, info.Mode().Perm()); err != nil {
			return err
		}
	}

	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// A leftover WAL belongs to the old file and would be replayed onto the new one.
	// It is empty here: the checkpoint above proved every frame reached the main file.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

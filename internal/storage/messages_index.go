package storage

import (
	"database/sql"
	"fmt"
	"log"
)

// messageSequenceIndex is the (session_id, sequence) index on messages. The
// sequence number identifies a message's position in its session, so the index
// has to be UNIQUE: two writers on the same session — the watcher pool next to
// the startup scan, or two daemons overlapping during a deploy — can otherwise
// both insert the same sequence. Duplicates break the append watermark's
// contiguity check for good (the stored rows then never have the shape the
// watermark describes), so the invariant belongs in the schema, not only in the
// writers' bookkeeping.
const messageSequenceIndex = "idx_messages_session_seq"

// ensureUniqueMessageSequence upgrades messageSequenceIndex to a UNIQUE index
// and removes the rows that would violate it.
//
// It replaces the plain (session_id, sequence) index that earlier releases
// created. Two reasons it is a guard and not a blind statement next to the
// other migrations: the duplicate purge is a full pass over messages and must
// not run on every start, and a unique CREATE silently ignored (the migration
// loops swallow errors) on a database that still holds duplicates would leave
// the schema looking fixed while the invariant is not there.
//
// Callers run it after the messages table exists: messages.db (the real store)
// and yesmem.db, which carries a mirror of the table. It is cheap on a
// migrated database — one index_list read — and does the copy-once work (purge,
// index rebuild) only where an old, non-unique index is found.
func ensureUniqueMessageSequence(db *sql.DB) error {
	// BEGIN IMMEDIATE rather than a deferred transaction: the write lock has to be
	// held before the index list is read. Every process that opens the store runs
	// this guard — the daemon and each hook invocation — so without the lock a
	// second process can repair the index between the check and the DDL, and the
	// loser aborts on a DROP or CREATE it no longer has any business doing.
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate: %w", err)
	}
	done := false
	defer func() {
		if !done {
			db.Exec("ROLLBACK")
		}
	}()

	exists, unique, err := indexIsUnique(db, "messages", messageSequenceIndex)
	if err != nil {
		return err
	}
	if exists && unique {
		return commit(db, &done)
	}
	if exists {
		if _, err := db.Exec("DROP INDEX IF EXISTS " + messageSequenceIndex); err != nil {
			return fmt.Errorf("drop non-unique %s: %w", messageSequenceIndex, err)
		}
	}
	removed, err := purgeDuplicateMessageSequences(db)
	if err != nil {
		return err
	}
	if _, err := db.Exec("CREATE UNIQUE INDEX " + messageSequenceIndex +
		" ON messages(session_id, sequence)"); err != nil {
		return fmt.Errorf("create unique %s: %w", messageSequenceIndex, err)
	}
	if err := commit(db, &done); err != nil {
		return err
	}
	log.Printf("storage: %s is now UNIQUE (%d duplicate rows removed)", messageSequenceIndex, removed)
	return nil
}

// commit ends the transaction and marks it as finished so the deferred rollback
// does not fire.
func commit(db *sql.DB, done *bool) error {
	if _, err := db.Exec("COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	*done = true
	return nil
}

// indexIsUnique reports whether index exists on table and enforces uniqueness.
// PRAGMA index_list yields (seq, name, unique, origin, partial) per index.
func indexIsUnique(db *sql.DB, table, index string) (exists, unique bool, err error) {
	rows, err := db.Query("PRAGMA index_list(" + table + ")")
	if err != nil {
		return false, false, fmt.Errorf("index_list(%s): %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var seq, uniq, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &uniq, &origin, &partial); err != nil {
			return false, false, fmt.Errorf("scan index_list(%s): %w", table, err)
		}
		if name == index {
			return true, uniq == 1, nil
		}
	}
	return false, false, rows.Err()
}

// purgeDuplicateMessageSequences keeps the newest row of every
// (session_id, sequence) pair and drops the rest, reporting how many rows went.
// Newest means highest row id: a duplicate is inserted later than the row it
// duplicates, and the later insert is the one whose watermark or trim decision is
// current.
func purgeDuplicateMessageSequences(db *sql.DB) (int64, error) {
	const duplicateIDs = `SELECT id FROM messages WHERE id NOT IN (
		SELECT MAX(id) FROM messages GROUP BY session_id, sequence)`

	// Search joins messages_fts to messages, so a leftover FTS row of a dropped
	// duplicate is invisible to search — it is dead weight, not a wrong answer.
	// Removing it still needs the table to exist (yesmem.db's mirror table has
	// no FTS side).
	if tableExists(db, "messages_fts") {
		if _, err := db.Exec("DELETE FROM messages_fts WHERE rowid IN (" + duplicateIDs + ")"); err != nil {
			return 0, fmt.Errorf("purge duplicate fts rows: %w", err)
		}
	}
	res, err := db.Exec("DELETE FROM messages WHERE id IN (" + duplicateIDs + ")")
	if err != nil {
		return 0, fmt.Errorf("purge duplicate messages: %w", err)
	}
	removed, _ := res.RowsAffected()
	return removed, nil
}

// tableExists reports whether a table (or virtual table) of that name exists.
func tableExists(db *sql.DB, name string) bool {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

package repair

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/carsteneu/yesmem/internal/textutil"
)

// messageFTSText is the text a message contributes to the search index. It is the
// same expression the write path applies (storage.insertMessagesTx copies a
// thinking block's blob into content before it inserts): a row written before
// that fallback existed carries its text only in content_blob.
const messageFTSText = `COALESCE(NULLIF(m.content, ''), CAST(m.content_blob AS TEXT), '')`

// messageFTSIndexable is the predicate a backfillable row has to meet. A row with
// neither content nor blob has nothing to index; the write path skips it too, and
// an FTS row for it would only make the index pay for empty terms.
const messageFTSIndexable = messageFTSText + ` <> ''`

// messageFTSMissing selects the rows message_search cannot see: SearchMessages
// joins messages_fts to messages, so a message without an FTS row is invisible no
// matter what its content holds.
const messageFTSMissing = `NOT EXISTS (SELECT 1 FROM messages_fts f WHERE f.rowid = m.id)`

// MessagesFTSReport is what the dry run found in one messages.db.
type MessagesFTSReport struct {
	DBPath       string
	Messages     int64
	Missing      int64
	Backfillable int64
	Empty        int64
	OrphanFTS    int64
	Examples     []MessagesFTSSample
}

// MessagesFTSSample is one row an apply would index.
type MessagesFTSSample struct {
	ID          int64
	SessionID   string
	MessageType string
	Timestamp   string
	Snippet     string
}

// PlanMessagesFTS reports how many messages of dbPath are missing from
// messages_fts and how many of them the backfill can index. It opens the database
// read-only and writes nothing: the apply is a separate, explicit call.
func PlanMessagesFTS(dbPath string, examples int) (*MessagesFTSReport, error) {
	if err := requireMessagesDB(dbPath); err != nil {
		return nil, err
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	report := &MessagesFTSReport{DBPath: dbPath}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&report.Messages); err != nil {
		return nil, fmt.Errorf("count messages: %w", err)
	}

	// One pass: a separate count per bucket would scan the 10 GB store once per
	// bucket, and the live table makes each pass expensive.
	if err := db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN `+messageFTSIndexable+` THEN 1 ELSE 0 END), 0)
		FROM messages m WHERE `+messageFTSMissing).Scan(&report.Missing, &report.Backfillable); err != nil {
		return nil, fmt.Errorf("count rows without an fts entry: %w", err)
	}
	report.Empty = report.Missing - report.Backfillable

	// Orphans are invisible to search as well (the join drops them), so the tool
	// reports them but leaves them alone: deleting FTS rows is not part of a
	// backfill.
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts f
		WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = f.rowid)`).Scan(&report.OrphanFTS); err != nil {
		return nil, fmt.Errorf("count orphan fts rows: %w", err)
	}

	if examples > 0 {
		samples, err := messageFTSSamples(db, examples)
		if err != nil {
			return nil, err
		}
		report.Examples = samples
	}
	return report, nil
}

// messageFTSBatchSize bounds one write transaction of the backfill. Small enough
// that the write lock is never held during a table scan, large enough that the
// candidate read is not a round trip per row.
const messageFTSBatchSize = 500

// ApplyMessagesFTS inserts the missing messages_fts rows and returns how many it
// wrote. Only the two conditions of the report qualify it, so a second run inserts
// nothing. The daemon may write to the same file throughout: every insert carries
// its own NOT EXISTS guard, and the candidate read runs outside a write
// transaction, so no lock is held while the table is scanned.
func ApplyMessagesFTS(dbPath string) (int64, error) {
	if err := requireMessagesDB(dbPath); err != nil {
		return 0, err
	}
	db, err := openForApply(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var afterID int64
	var inserted int64
	for {
		candidates, err := messageFTSCandidates(db, afterID, messageFTSBatchSize)
		if err != nil {
			return inserted, err
		}
		if len(candidates) == 0 {
			return inserted, nil
		}
		afterID = candidates[len(candidates)-1].ID
		written, err := insertMessageFTSRows(db, candidates)
		if err != nil {
			return inserted, err
		}
		inserted += written
	}
}

// messageFTSRow is one row the backfill writes: the message id becomes the FTS
// rowid.
type messageFTSRow struct {
	ID   int64
	Text string
}

func messageFTSCandidates(db *sql.DB, afterID int64, limit int) ([]messageFTSRow, error) {
	rows, err := db.Query(`SELECT m.id, `+messageFTSText+`
		FROM messages m
		WHERE m.id > ? AND `+messageFTSIndexable+` AND `+messageFTSMissing+`
		ORDER BY m.id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("select messages without an fts entry: %w", err)
	}
	defer rows.Close()

	var out []messageFTSRow
	for rows.Next() {
		var r messageFTSRow
		if err := rows.Scan(&r.ID, &r.Text); err != nil {
			return nil, fmt.Errorf("scan message row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// insertMessageFTSRows writes one batch in its own transaction. The guards sit in
// the statement rather than in the candidate query: a writer that indexed one of
// these rows between the read and this insert must not get a second, duplicate FTS
// row, and one that deleted a message must not get an FTS row for a row that no
// longer exists. The second case is the orphan shape the dry run reports, and a
// backfill has no business creating it.
//
// The count is only returned after a successful commit: a rolled back batch must
// not show up in the number the tool prints as its result.
func insertMessageFTSRows(db *sql.DB, rows []messageFTSRow) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO messages_fts(rowid, content)
		SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM messages_fts WHERE rowid = ?)
		AND EXISTS (SELECT 1 FROM messages WHERE id = ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare fts insert: %w", err)
	}
	defer stmt.Close()

	var inserted int64
	for _, r := range rows {
		res, err := stmt.Exec(r.ID, r.Text, r.ID, r.ID)
		if err != nil {
			return 0, fmt.Errorf("insert fts row for message %d: %w", r.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("rows affected for message %d: %w", r.ID, err)
		}
		inserted += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return inserted, nil
}

// messageFTSSamples returns a few of the rows an apply would index. The snippet is
// taken wide and flattened in Go: message text is model output and may carry
// control characters, so it must not reach a terminal unsanitized.
func messageFTSSamples(db *sql.DB, limit int) ([]MessagesFTSSample, error) {
	// No ORDER BY: the interesting sample is a row that exists, and sorting the
	// whole table to pick a few would cost another full pass. The scan stops at
	// the first matches.
	rows, err := db.Query(`SELECT m.id, COALESCE(m.session_id, ''), COALESCE(m.message_type, ''),
			COALESCE(m.timestamp, ''), substr(`+messageFTSText+`, 1, 300)
		FROM messages m
		WHERE `+messageFTSIndexable+` AND `+messageFTSMissing+`
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("select sample rows: %w", err)
	}
	defer rows.Close()

	var out []MessagesFTSSample
	for rows.Next() {
		var s MessagesFTSSample
		if err := rows.Scan(&s.ID, &s.SessionID, &s.MessageType, &s.Timestamp, &s.Snippet); err != nil {
			return nil, fmt.Errorf("scan sample row: %w", err)
		}
		s.Snippet = textutil.SingleLine(s.Snippet, 100)
		out = append(out, s)
	}
	return out, rows.Err()
}

// requireMessagesDB refuses a path that holds no database. Both openers would
// create an empty file, and a repair run that leaves one behind looks like a
// successful run against an empty store.
func requireMessagesDB(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no messages database at %s: %w", path, err)
	}
	return nil
}

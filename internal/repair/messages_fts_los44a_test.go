package repair

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los44aMessagesPath builds a real messages.db under t.TempDir (schema from the
// store, not a hand-written subset) and returns its path.
func los44aMessagesPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "yesmem.db"))
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return filepath.Join(dir, "messages.db")
}

// los44aSeedMessage inserts a message row without its FTS row, which is the
// state pre-2b6886b9 rows are in: the write path indexed only non-empty content,
// so a thinking blob stayed out of messages_fts.
func los44aSeedMessage(t *testing.T, dbPath string, seq int, msgType, content string, blob []byte) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	res, err := db.Exec(`INSERT INTO messages
		(session_id, source_agent, role, message_type, content, content_blob, tool_name, file_path, timestamp, sequence, model)
		VALUES ('ses-los44a', 'claude', 'assistant', ?, ?, ?, '', '', '2026-10-20T17:00:00Z', ?, '')`,
		msgType, content, blob, seq)
	if err != nil {
		t.Fatalf("insert message seq=%d: %v", seq, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func los44aSeedFTS(t *testing.T, dbPath string, rowid int64, content string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO messages_fts(rowid, content) VALUES (?, ?)`, rowid, content); err != nil {
		t.Fatalf("insert fts rowid=%d: %v", rowid, err)
	}
}

func los44aCountFTS(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&n); err != nil {
		t.Fatalf("count fts: %v", err)
	}
	return n
}

// los44aInstallMessages lays out the four row shapes the report has to tell
// apart: text without FTS row, blob-only (a pre-fix thinking block) without FTS
// row, an empty row without FTS row, and one already indexed row.
func los44aInstallMessages(t *testing.T) (path string, blobID int64) {
	t.Helper()
	path = los44aMessagesPath(t)

	los44aSeedMessage(t, path, 1, "text", "los44a needle alpha", nil)
	blobID = los44aSeedMessage(t, path, 2, "thinking", "", []byte("los44a needle beta"))
	los44aSeedMessage(t, path, 3, "tool_result", "", nil)
	indexed := los44aSeedMessage(t, path, 4, "text", "los44a needle gamma", nil)
	los44aSeedFTS(t, path, indexed, "los44a needle gamma")
	return path, blobID
}

// TestPlanMessagesFTSClassifiesMissingRowsLos44a is the dry-run contract: it
// reports how many rows are invisible, separates the ones carrying text from the
// ones with nothing to index, and writes nothing.
func TestPlanMessagesFTSClassifiesMissingRowsLos44a(t *testing.T) {
	path, _ := los44aInstallMessages(t)
	before := los44aCountFTS(t, path)

	report, err := PlanMessagesFTS(path, 5)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if report.Messages != 4 {
		t.Errorf("Messages = %d, want 4", report.Messages)
	}
	if report.Missing != 3 {
		t.Errorf("Missing = %d, want 3 (three rows without a messages_fts row)", report.Missing)
	}
	if report.Backfillable != 2 {
		t.Errorf("Backfillable = %d, want 2 (one text row, one blob-only thinking row)", report.Backfillable)
	}
	if report.Empty != 1 {
		t.Errorf("Empty = %d, want 1 (the row with neither content nor blob)", report.Empty)
	}
	if report.OrphanFTS != 0 {
		t.Errorf("OrphanFTS = %d, want 0 on a consistent table", report.OrphanFTS)
	}
	if len(report.Examples) == 0 {
		t.Fatalf("Examples is empty: the dry run has to show what it would insert")
	}
	if got := los44aCountFTS(t, path); got != before {
		t.Fatalf("messages_fts holds %d rows after the dry run, want %d: the report must not write", got, before)
	}
}

// TestApplyMessagesFTSBackfillsOnlyIndexableTextLos44a: the apply inserts the
// missing rows, indexes a thinking block from its blob (the pre-fix rows carry no
// content), leaves the empty row alone, and is idempotent.
func TestApplyMessagesFTSBackfillsOnlyIndexableTextLos44a(t *testing.T) {
	path, blobID := los44aInstallMessages(t)

	inserted, err := ApplyMessagesFTS(path)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if inserted != 2 {
		t.Fatalf("inserted = %d, want 2 (text row + blob-only thinking row)", inserted)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, id := range []int64{1, 4} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count fts row %d: %v", id, err)
		}
		if n != 1 {
			t.Errorf("messages_fts has %d rows for message %d, want 1", n, id)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = 3`).Scan(&n); err != nil {
		t.Fatalf("count fts row 3: %v", err)
	}
	if n != 0 {
		t.Errorf("messages_fts has a row for the empty message 3: nothing to index, the write path skips it too")
	}

	// The backfilled blob row has to be reachable through the store's own search,
	// which is the whole point of the backfill.
	store, err := storage.Open(filepath.Join(filepath.Dir(path), "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	hits, err := store.SearchMessages("needle", 20)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	foundBlob := false
	for _, hit := range hits {
		if hit.ID == blobID {
			foundBlob = true
		}
	}
	if !foundBlob {
		t.Fatalf("search does not return the backfilled thinking row %d (hits: %+v); its text lives only in content_blob", blobID, hits)
	}

	again, err := ApplyMessagesFTS(path)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if again != 0 {
		t.Fatalf("second apply inserted %d rows, want 0: the backfill has to be idempotent", again)
	}
}

// TestPlanMessagesFTSReportsOrphansLos44aLos44a: orphan FTS rows are invisible to
// search (it joins messages) but the operator should see the count.
func TestPlanMessagesFTSReportsOrphansLos44a(t *testing.T) {
	path, _ := los44aInstallMessages(t)
	los44aSeedFTS(t, path, 900001, "los44a orphan row without a message")

	report, err := PlanMessagesFTS(path, 0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if report.OrphanFTS != 1 {
		t.Errorf("OrphanFTS = %d, want 1", report.OrphanFTS)
	}
	if report.Missing != 3 {
		t.Errorf("Missing = %d, want 3: orphans must not change the missing count", report.Missing)
	}
}

// TestApplyMessagesFTSRefusesMissingDatabaseLos44a: a repair run must not leave a
// fresh empty database behind on a path that holds no messages.db.
func TestApplyMessagesFTSRefusesMissingDatabaseLos44a(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")

	if _, err := ApplyMessagesFTS(path); err == nil {
		t.Fatalf("ApplyMessagesFTS on a missing file returned no error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ApplyMessagesFTS created %s: a repair tool must not create a database", path)
	}
}

// TestInsertMessageFTSRowsGuardsLos44a pins the two guards that live inside the
// INSERT statement. The candidate query already filters both cases, so no
// end-to-end run reaches them; without this test the protection against a
// concurrent writer would be unproven.
func TestInsertMessageFTSRowsGuardsLos44a(t *testing.T) {
	path := los44aMessagesPath(t)
	keptID := los44aSeedMessage(t, path, 1, "text", "los44a guard kept", nil)
	goneID := los44aSeedMessage(t, path, 2, "text", "los44a guard gone", nil)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()

	rows := []messageFTSRow{{ID: keptID, Text: "los44a guard kept"}}
	first, err := insertMessageFTSRows(db, rows)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if first != 1 {
		t.Fatalf("first insert wrote %d rows, want 1", first)
	}

	// The message already has an FTS row: inserting again must write nothing.
	second, err := insertMessageFTSRows(db, rows)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if second != 0 {
		t.Fatalf("second insert wrote %d rows, want 0: a duplicate FTS row must be impossible", second)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, keptID).Scan(&n); err != nil {
		t.Fatalf("count fts rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("messages_fts holds %d rows for message %d, want 1", n, keptID)
	}

	// A message deleted between the candidate read and the insert must not get an
	// FTS row: that is exactly the orphan shape the dry run reports.
	if _, err := db.Exec(`DELETE FROM messages WHERE id = ?`, goneID); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	ghost, err := insertMessageFTSRows(db, []messageFTSRow{{ID: goneID, Text: "los44a guard gone"}})
	if err != nil {
		t.Fatalf("insert for a deleted message: %v", err)
	}
	if ghost != 0 {
		t.Fatalf("insert wrote %d rows for a deleted message, want 0", ghost)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, goneID).Scan(&n); err != nil {
		t.Fatalf("count orphan fts rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("messages_fts holds %d rows for the deleted message %d, want 0", n, goneID)
	}
}

// TestApplyMessagesFTSCrossesBatchBoundaryLos44a: the walk advances on the last id
// of each batch, so a table larger than one batch must not lose its tail.
func TestApplyMessagesFTSCrossesBatchBoundaryLos44a(t *testing.T) {
	path := los44aMessagesPath(t)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	const total = messageFTSBatchSize + 1
	for seq := 1; seq <= total; seq++ {
		if _, err := tx.Exec(`INSERT INTO messages
			(session_id, source_agent, role, message_type, content, content_blob, tool_name, file_path, timestamp, sequence, model)
			VALUES ('ses-batch', 'claude', 'assistant', 'text', 'los44a batch row', NULL, '', '', '2026-10-20T17:00:00Z', ?, '')`,
			seq); err != nil {
			tx.Rollback()
			db.Close()
			t.Fatalf("insert batch row %d: %v", seq, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	db.Close()

	inserted, err := ApplyMessagesFTS(path)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if inserted != total {
		t.Fatalf("inserted = %d, want %d: the id walk has to cross the batch boundary", inserted, total)
	}
	if got := los44aCountFTS(t, path); got != total {
		t.Fatalf("messages_fts holds %d rows, want %d", got, total)
	}
}

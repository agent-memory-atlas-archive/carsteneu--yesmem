package storage

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

func seedSessionMessages(t *testing.T, s *Store, sessionID string, bodies ...string) {
	t.Helper()
	if err := s.UpsertSession(&models.Session{
		ID:        sessionID,
		Project:   "/proj/incremental",
		StartedAt: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC),
		IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	msgs := make([]models.Message, 0, len(bodies))
	for i, b := range bodies {
		msgs = append(msgs, models.Message{
			SessionID:   sessionID,
			Role:        "user",
			MessageType: "text",
			Content:     b,
			Timestamp:   time.Date(2026, 10, 14, 9, i, 0, 0, time.UTC),
			Sequence:    i,
		})
	}
	if err := s.InsertMessages(msgs); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
}

// The append path runs two session-scoped statements per reindex: the message
// stats precondition (COUNT/MIN/MAX over one session) and the tail trim
// (sequence >= watermark). Without an index on (session_id, sequence) both
// degrade into a scan over every message of that session — a cost that grows
// with the session and quietly undoes the point of incremental indexing.
func TestMessagesSessionSequenceIndexExists(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Both handles: messages.db carries the real rows, yesmem.db mirrors the
	// legacy schema and is kept in sync by convention.
	for name, db := range map[string]*sql.DB{
		"messages.db": s.messagesReaderDB(),
		"yesmem.db":   s.readerDB(),
	} {
		var got string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_messages_session_seq'`).Scan(&got)
		if err != nil {
			t.Errorf("%s: index idx_messages_session_seq missing: %v", name, err)
		}
	}

	// Informational: with rows present the stats query should be index-backed.
	if err := s.UpsertSession(&models.Session{
		ID: "idx-probe", Project: "/proj", StartedAt: time.Now().UTC(), IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	msgs := make([]models.Message, 0, 50)
	for i := 0; i < 50; i++ {
		msgs = append(msgs, models.Message{
			SessionID: "idx-probe", Role: "user", MessageType: "text",
			Content: "row", Sequence: i, Timestamp: time.Now().UTC(),
		})
	}
	if err := s.InsertMessages(msgs); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	rows, err := s.messagesReaderDB().Query(
		`EXPLAIN QUERY PLAN SELECT COUNT(*), COALESCE(MIN(sequence),0), COALESCE(MAX(sequence),-1)
		 FROM messages WHERE session_id = ?`, "idx-probe")
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		t.Logf("query plan: %s", detail)
	}
}

// DeleteMessagesFromSequence trims the tail of a session: it removes messages
// with sequence >= fromSeq and their FTS rows, leaving earlier messages intact.
func TestDeleteMessagesFromSequence(t *testing.T) {
	s := mustOpen(t)
	seedSessionMessages(t, s, "sess-trim", "alpha_trim", "beta_trim", "gamma_trim", "delta_trim")

	if err := s.DeleteMessagesFromSequence("sess-trim", 2); err != nil {
		t.Fatalf("delete from sequence: %v", err)
	}

	msgs, err := s.GetMessagesBySession("sess-trim")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("remaining messages: got %d, want 2", len(msgs))
	}
	for _, m := range msgs {
		if m.Sequence >= 2 {
			t.Errorf("sequence %d survived the trim", m.Sequence)
		}
	}

	// FTS rows of the trimmmed messages must be gone as well.
	for _, token := range []string{"alpha_trim", "beta_trim"} {
		hits, err := s.SearchMessages(token, 10)
		if err != nil {
			t.Fatalf("search %s: %v", token, err)
		}
		if len(hits) != 1 {
			t.Errorf("FTS %s: got %d hits, want 1", token, len(hits))
		}
	}
	for _, token := range []string{"gamma_trim", "delta_trim"} {
		hits, err := s.SearchMessages(token, 10)
		if err != nil {
			t.Fatalf("search %s: %v", token, err)
		}
		if len(hits) != 0 {
			t.Errorf("FTS %s: got %d hits, want 0 (fts ghost left behind)", token, len(hits))
		}
	}
}

// Deleting from sequence 0 must equal a full session wipe.
func TestDeleteMessagesFromSequenceZero(t *testing.T) {
	s := mustOpen(t)
	seedSessionMessages(t, s, "sess-wipe", "one_wipe", "two_wipe")

	if err := s.DeleteMessagesFromSequence("sess-wipe", 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	msgs, err := s.GetMessagesBySession("sess-wipe")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("messages left: %d, want 0", len(msgs))
	}
}

// indexUniqueFlag reports whether the named index exists on table and whether
// it enforces uniqueness. PRAGMA index_list yields (seq, name, unique, origin,
// partial) for every index SQLite knows about on the table.
func indexUniqueFlag(t *testing.T, db *sql.DB, table, index string) (exists, unique bool) {
	t.Helper()
	rows, err := db.Query("PRAGMA index_list(" + table + ")")
	if err != nil {
		t.Fatalf("index_list(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq, uniq, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &uniq, &origin, &partial); err != nil {
			t.Fatalf("scan index_list(%s): %v", table, err)
		}
		if name == index {
			return true, uniq == 1
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index_list(%s): %v", table, err)
	}
	return false, false
}

// seedLegacyMessagesDB writes the pre-fix state of messages.db: a plain
// (non-unique) sequence index plus the duplicate rows two concurrent writers
// leave behind. Written before Open so the migration has something to repair.
func seedLegacyMessagesDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy messages db: %v", err)
	}
	defer db.Close()

	legacy := []string{
		tableSessions,
		tableMessages,
		`CREATE INDEX idx_messages_session_seq ON messages(session_id, sequence)`,
		`CREATE VIRTUAL TABLE messages_fts USING fts5(content, tokenize='unicode61', content_rowid=id)`,
		`INSERT INTO sessions (id, project, project_short, started_at, jsonl_path, indexed_at)
			VALUES ('sess-dup', '/proj/dup', '/proj/dup', '2026-10-14T09:00:00Z', '/proj/dup/s.jsonl', '2026-10-14T09:00:00Z')`,
	}
	for _, stmt := range legacy {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed legacy schema (%s): %v", stmt, err)
		}
	}

	// Two interleaved writers: sequence 1 exists twice, the second insert wins.
	for _, row := range []struct {
		seq     int
		content string
	}{{0, "seq0_legacy"}, {1, "seq1_stale"}, {1, "seq1_kept"}, {2, "seq2_legacy"}} {
		res, err := db.Exec(`INSERT INTO messages (session_id, role, message_type, content, tool_name, file_path, timestamp, sequence, model)
			VALUES ('sess-dup', 'user', 'text', ?, '', '', '2026-10-14T09:00:00Z', ?, '')`, row.content, row.seq)
		if err != nil {
			t.Fatalf("seed legacy row seq=%d: %v", row.seq, err)
		}
		id, _ := res.LastInsertId()
		if _, err := db.Exec(`INSERT INTO messages_fts(rowid, content) VALUES (?, ?)`, id, row.content); err != nil {
			t.Fatalf("seed legacy fts seq=%d: %v", row.seq, err)
		}
	}
}

// The (session_id, sequence) index must be UNIQUE: two writers on one session
// (the watcher pool and the startup scan, or two daemons during a deploy) would
// otherwise both insert the same sequence, and the duplicates make the append
// watermark's contiguity check fail for good. A store that was written before
// the index was unique has to be repaired on open: duplicates removed (newest
// row wins) so the unique index can be created at all.
func TestMessagesSequenceIndexIsUniqueAndLegacyDuplicatesPurged(t *testing.T) {
	dir := t.TempDir()
	messagesPath := filepath.Join(dir, "messages.db")
	seedLegacyMessagesDB(t, messagesPath)

	s, err := Open(filepath.Join(dir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if exists, unique := indexUniqueFlag(t, s.messagesReaderDB(), "messages", "idx_messages_session_seq"); !exists || !unique {
		t.Errorf("messages.db: idx_messages_session_seq exists=%v unique=%v, want true/true", exists, unique)
	}
	// yesmem.db carries a legacy mirror of the messages table; it must not drift
	// from messages.db, or the two schemas disagree about the same invariant.
	if exists, unique := indexUniqueFlag(t, s.readerDB(), "messages", "idx_messages_session_seq"); !exists || !unique {
		t.Errorf("yesmem.db: idx_messages_session_seq exists=%v unique=%v, want true/true", exists, unique)
	}

	msgs, err := s.GetMessagesBySession("sess-dup")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages after purge: got %d, want 3", len(msgs))
	}
	for i, m := range msgs {
		if m.Sequence != i {
			t.Errorf("message %d: sequence %d, want %d", i, m.Sequence, i)
		}
	}
	if msgs[1].Content != "seq1_kept" {
		t.Errorf("duplicate survivor: got %q, want the newest row %q", msgs[1].Content, "seq1_kept")
	}

	// The FTS row of the dropped duplicate must go too, or the search index keeps
	// answering for a message that no longer exists.
	if hits, err := s.SearchMessages("seq1_stale", 10); err != nil {
		t.Fatalf("search stale duplicate: %v", err)
	} else if len(hits) != 0 {
		t.Errorf("stale duplicate still searchable: %d hits, want 0", len(hits))
	}
	if hits, err := s.SearchMessages("seq1_kept", 10); err != nil {
		t.Fatalf("search surviving row: %v", err)
	} else if len(hits) != 1 {
		t.Errorf("surviving row: %d hits, want 1", len(hits))
	}
}

// A duplicate (session_id, sequence) insert must fail as a whole: the batch is
// one transaction, so a partially inserted tail must not survive a rejected
// run — the append path relies on the failure to fall back to a full reindex.
func TestInsertMessagesRejectsDuplicateSequence(t *testing.T) {
	s := mustOpen(t)
	seedSessionMessages(t, s, "sess-unique", "first_unique", "second_unique")

	err := s.InsertMessages([]models.Message{
		{SessionID: "sess-unique", Role: "user", MessageType: "text", Content: "dup_unique",
			Timestamp: time.Date(2026, 10, 14, 9, 5, 0, 0, time.UTC), Sequence: 1},
		{SessionID: "sess-unique", Role: "user", MessageType: "text", Content: "tail_unique",
			Timestamp: time.Date(2026, 10, 14, 9, 6, 0, 0, time.UTC), Sequence: 2},
	})
	if err == nil {
		t.Fatal("duplicate sequence insert succeeded, want error")
	}

	msgs, err := s.GetMessagesBySession("sess-unique")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages after rejected insert: got %d, want 2 (batch must roll back)", len(msgs))
	}
	for _, m := range msgs {
		if m.Sequence == 2 {
			t.Errorf("sequence 2 survived a rejected batch: %q", m.Content)
		}
	}
}

// Every process that opens the store runs the index guard — the daemon and every
// hook invocation — so two of them can race on the same database: one repairs the
// index while the other is still reading the index list. The guard takes the
// write lock before that read, which is what makes the loser wait for the winner
// instead of aborting on a DROP or CREATE the winner already did.
func TestEnsureUniqueMessageSequenceUnderConcurrentCallers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	seedLegacyMessagesDB(t, path)

	const callers = 4
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := sql.Open("sqlite", path)
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err := db.Exec("PRAGMA busy_timeout=30000"); err != nil {
				errs[i] = err
				return
			}
			errs[i] = ensureUniqueMessageSequence(db)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent guard call %d: %v", i, err)
		}
	}

	s, err := Open(filepath.Join(dir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if exists, unique := indexUniqueFlag(t, s.messagesReaderDB(), "messages", "idx_messages_session_seq"); !exists || !unique {
		t.Errorf("after the race: index exists=%v unique=%v, want true/true", exists, unique)
	}
	if got := len(mustMessages(t, s, "sess-dup")); got != 3 {
		t.Errorf("rows after the race: got %d, want 3 (the purge ran exactly once)", got)
	}
}

// mustMessages reads a session's stored messages.
func mustMessages(t *testing.T, s *Store, sessionID string) []models.Message {
	t.Helper()
	msgs, err := s.GetMessagesBySession(sessionID)
	if err != nil {
		t.Fatalf("get messages for %s: %v", sessionID, err)
	}
	return msgs
}

// Two processes can repair the same database at once: every hook invocation opens
// the store. The guard takes the write lock before it reads the index list, so a
// caller that arrives second waits for the first instead of deciding from a stale
// read — otherwise it drops an index that is already gone or creates one that
// already exists, and fails. Here the competing writer holds the write lock,
// performs the repair and commits after the guard has done its read.
func TestEnsureUniqueMessageSequenceWaitsForAConcurrentRepair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	seedLegacyMessagesDB(t, path)

	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open competing handle: %v", err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	if _, err := other.Exec("PRAGMA busy_timeout=30000"); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if _, err := other.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("competing begin: %v", err)
	}

	// Open the guard's handle up front: its own connection setup would otherwise
	// outlast the window this test is trying to create.
	guarded, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open guarded handle: %v", err)
	}
	defer guarded.Close()
	guarded.SetMaxOpenConns(1)
	if _, err := guarded.Exec("PRAGMA busy_timeout=30000"); err != nil {
		t.Fatalf("guarded busy_timeout: %v", err)
	}
	var probe int
	if err := guarded.QueryRow("SELECT COUNT(*) FROM messages").Scan(&probe); err != nil {
		t.Fatalf("warm up guarded connection: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- ensureUniqueMessageSequence(guarded)
	}()

	// Give the guard time to reach its index_list read — a plain read, so it does
	// not wait for the write lock — then repair from here and commit.
	time.Sleep(200 * time.Millisecond)
	repair := []string{
		`DELETE FROM messages_fts WHERE rowid IN (SELECT id FROM messages WHERE id NOT IN (
			SELECT MAX(id) FROM messages GROUP BY session_id, sequence))`,
		`DELETE FROM messages WHERE id NOT IN (SELECT MAX(id) FROM messages GROUP BY session_id, sequence)`,
		`DROP INDEX IF EXISTS idx_messages_session_seq`,
		`CREATE UNIQUE INDEX idx_messages_session_seq ON messages(session_id, sequence)`,
	}
	for _, stmt := range repair {
		if _, err := other.Exec(stmt); err != nil {
			t.Fatalf("competing repair (%s): %v", stmt, err)
		}
	}
	if _, err := other.Exec("COMMIT"); err != nil {
		t.Fatalf("competing commit: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("guard failed although another process had already repaired the index: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("guard did not return")
	}
}

// The repair deletes rows, so it has to be all-or-nothing: if the unique index
// cannot be created, the duplicates must still be there. Without the surrounding
// transaction the purge committed first and a failing CREATE left the rows gone
// with nothing to roll back to — and the failure then keeps the daemon from
// starting, so it would loop over an already-modified database.
func TestEnsureUniqueMessageSequenceRollsBackWhenTheIndexCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	seedLegacyMessagesDB(t, path)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open messages db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Free the name and take it with a table: index and table names share one
	// namespace, so CREATE UNIQUE INDEX now fails for a reason the repair cannot
	// work around.
	for _, stmt := range []string{
		`DROP INDEX idx_messages_session_seq`,
		`CREATE TABLE idx_messages_session_seq (placeholder TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("test setup (%s): %v", stmt, err)
		}
	}

	if err := ensureUniqueMessageSequence(db); err == nil {
		t.Fatal("repair succeeded although the index name was taken, want error")
	}

	// The duplicates are still there, and so are their FTS rows.
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = 'sess-dup'`).Scan(&rows); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if rows != 4 {
		t.Errorf("messages after the failed repair: got %d, want the original 4", rows)
	}
	var fts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&fts); err != nil {
		t.Fatalf("count fts: %v", err)
	}
	if fts != 4 {
		t.Errorf("fts rows after the failed repair: got %d, want the original 4", fts)
	}
}

// Replacing a session's messages is one transaction: the old rows only go if the
// new ones land. A rejected batch (here: a duplicate sequence) must leave the
// previous messages untouched instead of an empty session — the indexer's full
// pass relies on that when the unique index turns an interleaved write away.
func TestReplaceMessagesBySessionIsAtomic(t *testing.T) {
	s := mustOpen(t)
	seedSessionMessages(t, s, "sess-replace", "old_one_replace", "old_two_replace")

	err := s.ReplaceMessagesBySession("sess-replace", []models.Message{
		{SessionID: "sess-replace", Role: "user", MessageType: "text", Content: "new_one_replace",
			Timestamp: time.Date(2026, 10, 14, 9, 10, 0, 0, time.UTC), Sequence: 0},
		{SessionID: "sess-replace", Role: "user", MessageType: "text", Content: "new_two_replace",
			Timestamp: time.Date(2026, 10, 14, 9, 11, 0, 0, time.UTC), Sequence: 0},
	})
	if err == nil {
		t.Fatal("replacing with a duplicate sequence succeeded, want error")
	}

	msgs, err := s.GetMessagesBySession("sess-replace")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages after rejected replace: got %d, want the 2 old rows", len(msgs))
	}
	for i, want := range []string{"old_one_replace", "old_two_replace"} {
		if msgs[i].Content != want {
			t.Errorf("row %d: got %q, want %q", i, msgs[i].Content, want)
		}
	}

	// And the FTS rows of the old messages must still be there.
	if hits, err := s.SearchMessages("old_one_replace", 10); err != nil {
		t.Fatalf("search old row: %v", err)
	} else if len(hits) != 1 {
		t.Errorf("old row lost from FTS: %d hits, want 1", len(hits))
	}
}

// A trim with no matching rows must be a silent no-op (crash-recovery path
// runs it unconditionally before every append insert).
func TestDeleteMessagesFromSequenceNoop(t *testing.T) {
	s := mustOpen(t)
	seedSessionMessages(t, s, "sess-noop", "keep_noop")

	if err := s.DeleteMessagesFromSequence("sess-noop", 5); err != nil {
		t.Fatalf("delete: %v", err)
	}
	msgs, err := s.GetMessagesBySession("sess-noop")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("messages: got %d, want 1", len(msgs))
	}
}

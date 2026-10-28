package indexer

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// The fork moved session storage to session_v2/session_message (migration
// #40723) and stopped writing the legacy session/message/part tables. A scanner
// that keeps reading the legacy tables sees a database frozen at that migration
// and imports nothing: on this machine that was every opencode session since
// 12.10.2026, found on 28.10.
func TestOpencodeScannerReadsTheV2Tables(t *testing.T) {
	tmpDir := t.TempDir()
	ocDBPath := filepath.Join(tmpDir, "opencode.db")

	db, err := sql.Open("sqlite", ocDBPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE session_v2 (
			id TEXT PRIMARY KEY, project_id TEXT, workspace_id TEXT, parent_id TEXT,
			fork_session_id TEXT, fork_boundary TEXT, slug TEXT, directory TEXT, path TEXT,
			title TEXT, version TEXT, metadata TEXT, agent TEXT, model TEXT,
			time_created INTEGER, time_updated INTEGER
		)`,
		`CREATE TABLE session_message (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL,
			seq INTEGER NOT NULL, time_created INTEGER, time_updated INTEGER, data TEXT NOT NULL
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	sessID := "ses_v2only"
	created := time.Now().Add(-2 * time.Hour).UnixMilli()
	updated := time.Now().Add(-1 * time.Hour).UnixMilli()
	if _, err := db.Exec(`INSERT INTO session_v2
		(id, project_id, directory, title, agent, model, time_created, time_updated)
		VALUES (?, 'prj', '/home/test/v2project', 'V2 Session', 'build', 'gd-flash', ?, ?)`,
		sessID, created, updated); err != nil {
		t.Fatal(err)
	}

	mustInsert := func(id, typ string, seq int, data string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO session_message
			(id, session_id, type, seq, time_created, time_updated, data)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, sessID, typ, seq, created+int64(seq), created+int64(seq), data); err != nil {
			t.Fatal(err)
		}
	}
	mustInsert("sm_1", "user", 1, `{"time":{"created":1},"text":"hello v2","files":[],"agents":[]}`)
	mustInsert("sm_2", "assistant", 2, `{"time":{"created":2},"agent":"build","model":{"id":"gd-flash","providerID":"gateway"},"content":[{"type":"text","text":"I can help v2"},{"type":"tool","tool":"read","state":{"status":"completed","input":{"path":"a"},"output":"ok"}}]}`)
	// A row type the scanner must not turn into a message.
	mustInsert("sm_3", "idle", 3, `{"time":{"created":3}}`)

	// v2 allows a NULL title, the legacy table did not: a session without one
	// must not abort the whole scan.
	if _, err := db.Exec(`INSERT INTO session_v2
		(id, project_id, directory, title, agent, model, time_created, time_updated)
		VALUES ('ses_notitle', 'prj', '/home/test/v2project', NULL, '', '', ?, ?)`,
		updated+1000, updated+1000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_message
		(id, session_id, type, seq, time_created, time_updated, data)
		VALUES ('sm_9', 'ses_notitle', 'user', 1, ?, ?, '{"text":"no title here"}')`,
		updated+1000, updated+1000); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	scanner := NewOpencodeScanner(ocDBPath, store, nil, nil, nil, nil)
	scanner.lastCheckAt = time.Now().Add(-2 * scanOpencodeInterval)
	scanner.MaybeScan()

	normID := models.NormalizeSessionID(models.SourceAgentOpencode, sessID)
	sess, err := store.GetSession(normID)
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatalf("session %s not found after scanning the v2 tables", normID)
	}
	if sess.MessageCount < 2 {
		t.Fatalf("expected at least 2 messages from the v2 rows, got %d", sess.MessageCount)
	}

	msgs, err := store.GetMessagesBySession(normID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 stored messages, got %d", len(msgs))
	}
	if msgs[0].Content != "hello v2" {
		t.Fatalf("first message: %q, want %q", msgs[0].Content, "hello v2")
	}
	if msgs[0].Role != "user" {
		t.Fatalf("first message role: %q, want user", msgs[0].Role)
	}

	noTitle, err := store.GetSession(models.NormalizeSessionID(models.SourceAgentOpencode, "ses_notitle"))
	if err != nil {
		t.Fatal(err)
	}
	if noTitle == nil {
		t.Fatal("a session with a NULL title must still be indexed")
	}
}

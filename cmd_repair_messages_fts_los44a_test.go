package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los44aMessagesInstall builds an installation whose messages.db holds one text
// row and one blob-only thinking row, both without a messages_fts row, and
// returns the path of messages.db.
func los44aMessagesInstall(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	messagesDB := filepath.Join(dataDir, "messages.db")
	los38ySeedDB(t, messagesDB,
		`INSERT INTO messages (session_id, source_agent, role, message_type, content, content_blob, tool_name, file_path, timestamp, sequence, model)
			VALUES ('ses-los44a', 'claude', 'assistant', 'text', 'los44a needle alpha', NULL, '', '', '2026-10-20T17:00:00Z', 1, '')`,
		`INSERT INTO messages (session_id, source_agent, role, message_type, content, content_blob, tool_name, file_path, timestamp, sequence, model)
			VALUES ('ses-los44a', 'claude', 'assistant', 'thinking', '', 'los44a needle beta', '', '', '2026-10-20T17:00:01Z', 2, '')`)
	return messagesDB
}

func los44aFTSRows(t *testing.T, messagesDB string) int {
	t.Helper()
	db, err := sql.Open("sqlite", messagesDB)
	if err != nil {
		t.Fatalf("open %s: %v", messagesDB, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&n); err != nil {
		t.Fatalf("count fts: %v", err)
	}
	return n
}

// TestRepairMessagesFTSDefaultsToDryRunLos44a: the command reports what it would
// insert and writes nothing unless --apply is passed. An operator run against the
// live database must not change it by omission.
func TestRepairMessagesFTSDefaultsToDryRunLos44a(t *testing.T) {
	messagesDB := los44aMessagesInstall(t)

	var out bytes.Buffer
	if err := repairMessagesFTS(messagesDB, false, 5, &out); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := los44aFTSRows(t, messagesDB); got != 0 {
		t.Fatalf("messages_fts holds %d rows after the dry run, want 0", got)
	}
	text := out.String()
	if !strings.Contains(text, "dry run") {
		t.Errorf("dry run output does not say that nothing was written:\n%s", text)
	}
	if !strings.Contains(text, "backfillable: 2") {
		t.Errorf("dry run output does not report the two backfillable rows:\n%s", text)
	}
}

// TestRepairMessagesFTSApplyWritesLos44a: --apply inserts the missing rows and
// reports how many went in.
func TestRepairMessagesFTSApplyWritesLos44a(t *testing.T) {
	messagesDB := los44aMessagesInstall(t)

	var out bytes.Buffer
	if err := repairMessagesFTS(messagesDB, true, 5, &out); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := los44aFTSRows(t, messagesDB); got != 2 {
		t.Fatalf("messages_fts holds %d rows after the apply, want 2", got)
	}
	if !strings.Contains(out.String(), "inserted: 2") {
		t.Errorf("apply output does not report the inserted row count:\n%s", out.String())
	}
}

// TestRepairMessagesFTSRefusesMissingDatabaseLos44a: the command must not create a
// messages.db on a path that holds none.
func TestRepairMessagesFTSRefusesMissingDatabaseLos44a(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "messages.db")

	err := repairMessagesFTS(missing, true, 5, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("repairMessagesFTS on a missing file returned no error")
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("repairMessagesFTS created %s", missing)
	}
}

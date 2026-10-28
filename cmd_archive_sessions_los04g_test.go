package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// The session id listed under "left unwritten" comes from the sessions table. A
// newline in it must not pass the rest of the line off as its own output line
// (YM-332).
func TestRunArchiveSessions_QuotesTheIncompleteSessionID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	forgedID := "sess\nwarn: forged archive line"
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// No messages at all: the stored count cannot match, so the session is
	// reported as incomplete instead of written.
	if err := store.UpsertSession(&models.Session{
		ID:           forgedID,
		Project:      filepath.Join(home, "proj"),
		MessageCount: 3,
		StartedAt:    time.Now().Add(-time.Hour),
		EndedAt:      time.Now().Add(-time.Minute),
		IndexedAt:    time.Now(),
		SourceAgent:  models.SourceAgentOpencode,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	prevArgs, prevStdout := os.Args, os.Stdout
	t.Cleanup(func() {
		os.Args, os.Stdout = prevArgs, prevStdout
	})
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"yesmem", "archive-sessions", "--dry-run"}
	os.Stdout = writer
	runArchiveSessions()
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	os.Stdout = prevStdout
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	reader.Close()

	printed := string(out)
	if !strings.Contains(printed, "left unwritten") {
		t.Fatalf("the incomplete session was not reported: %q", printed)
	}
	if strings.Contains(printed, "\nwarn: forged") {
		t.Fatalf("a session id forged an output line:\n%s", printed)
	}
	if !strings.Contains(printed, "warn: forged archive line") {
		t.Fatalf("the session id is missing from the report:\n%s", printed)
	}
}

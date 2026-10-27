package indexer

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/models"
)

// The progress line of the backfill is written to stdout and to a caller's
// writer. Its format ("would archive <id>") is part of the command's output, so
// the id is not quoted; it must not carry a line end into that output either.
// Ids are generated internally, but the store can hold a row written by an older
// version or a foreign writer (YM-332).
func TestArchiveMissingSessions_DryRunProgressStaysOnOneLine(t *testing.T) {
	store := openBackfillStore(t)

	forgedID := "sess\nwarn: forged progress line\u00ad\u200e"
	sess := &models.Session{
		ID:           forgedID,
		Project:      filepath.Join(t.TempDir(), "proj"),
		MessageCount: 1,
		StartedAt:    time.Now().Add(-time.Hour),
		EndedAt:      time.Now().Add(-time.Minute),
		IndexedAt:    time.Now(),
		SourceAgent:  models.SourceAgentOpencode,
	}
	if err := store.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := store.InsertMessages([]models.Message{{
		SessionID:   forgedID,
		SourceAgent: models.SourceAgentOpencode,
		Role:        "user",
		MessageType: "text",
		Content:     "only listed",
		Timestamp:   sess.StartedAt,
		Sequence:    0,
	}}); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	var progress bytes.Buffer
	res, err := ArchiveMissingSessions(store, archive.New(filepath.Join(t.TempDir(), "archive")),
		ArchiveBackfillOptions{DryRun: true, Progress: &progress})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("dry run list: got %+v, want exactly the forged session", res.Written)
	}

	out := progress.String()
	if strings.Contains(out, "\nwarn: forged") {
		t.Fatalf("a session id forged a second progress line:\n%s", out)
	}
	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("progress output spans %d lines, want 1: %q", got, out)
	}
	// Invisible format characters must not reach the line either; the soft hyphen
	// and the left-to-right mark are format characters that a narrower set misses.
	if strings.ContainsAny(out, "\u00ad\u200e") {
		t.Fatalf("format characters survived: %q", out)
	}
	if !strings.HasPrefix(out, "would archive ") {
		t.Fatalf("progress output lost its pinned format: %q", out)
	}
}

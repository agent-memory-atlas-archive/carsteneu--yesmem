package main

import (
	"database/sql"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// The narrative cleanup runs on every regenerate-narratives. A swallowed error
// hides a broken cleanup for the whole run and leaves short-session narratives in
// place, so the failure has to reach the log.
func TestCleanupShortSessionNarratives_LogsError(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s.Close()

	var buf strings.Builder
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	if n := cleanupShortSessionNarratives(s); n != 0 {
		t.Errorf("cleaned %d narratives from a broken store, want 0", n)
	}
	out := buf.String()
	if !strings.Contains(out, "warn: clean up short-session narratives") {
		t.Errorf("cleanup error was not logged, log output: %q", out)
	}
	if !strings.Contains(out, "database is closed") {
		t.Errorf("log line does not name the cause, log output: %q", out)
	}
}

// The cleanup must keep the narratives of sessions the generator accepts (see
// internal/daemon/narrative.go:minNarrativeMessages, which is this same constant).
func TestCleanupShortSessionNarratives_KeepsExtractableSession(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	ended := time.Now().Add(-72 * time.Hour)
	if err := s.UpsertSession(&models.Session{
		ID: "sess-cli-extractable", Project: "/test", ProjectShort: "test",
		MessageCount: storage.ShortSessionMaxMessages,
		StartedAt:    ended.Add(-time.Minute), EndedAt: ended,
		JSONLPath: "/sess-cli-extractable.jsonl", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := s.MarkSessionNarrative("sess-cli-extractable"); err != nil {
		t.Fatalf("mark session narrative: %v", err)
	}
	id, err := s.InsertLearning(&models.Learning{
		Category: "narrative", Content: "Narrative of a session long enough to extract",
		SessionID: "sess-cli-extractable", Source: "llm_extracted", Confidence: 1.0,
		CreatedAt: time.Now(), ModelUsed: "self",
	})
	if err != nil {
		t.Fatalf("insert narrative: %v", err)
	}

	if n := cleanupShortSessionNarratives(s); n != 0 {
		t.Errorf("cleaned %d narratives of an extractable session, want 0", n)
	}

	var superseded sql.NullInt64
	if err := s.DB().QueryRow(`SELECT superseded_by FROM learnings WHERE id = ?`, id).Scan(&superseded); err != nil {
		t.Fatalf("read narrative: %v", err)
	}
	if superseded.Valid {
		t.Errorf("narrative of a %d-message session must stay active, superseded_by = %d",
			storage.ShortSessionMaxMessages, superseded.Int64)
	}
}

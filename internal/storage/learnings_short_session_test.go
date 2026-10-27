package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// seedShortSession inserts a session row with the given message count whose last
// activity lies endedAgo in the past.
func seedShortSession(t *testing.T, s *Store, id string, messages int, endedAgo time.Duration) {
	t.Helper()
	ended := time.Now().Add(-endedAgo)
	if err := s.UpsertSession(&models.Session{
		ID: id, Project: "/test", ProjectShort: "test",
		MessageCount: messages,
		StartedAt:    ended.Add(-time.Minute),
		EndedAt:      ended,
		JSONLPath:    "/" + id + ".jsonl",
		IndexedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
}

func insertSessionLearning(t *testing.T, s *Store, sessionID, source, content string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: content, SessionID: sessionID, Source: source,
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})
	if err != nil {
		t.Fatalf("insert learning for %s: %v", sessionID, err)
	}
	return id
}

type supersedeState struct {
	by         sql.NullInt64
	reason     sql.NullString
	validUntil sql.NullString
}

func readSupersedeState(t *testing.T, s *Store, id int64) supersedeState {
	t.Helper()
	var st supersedeState
	if err := s.DB().QueryRow(
		`SELECT superseded_by, supersede_reason, valid_until FROM learnings WHERE id = ?`, id,
	).Scan(&st.by, &st.reason, &st.validUntil); err != nil {
		t.Fatalf("read learning %d: %v", id, err)
	}
	return st
}

func requireActive(t *testing.T, s *Store, id int64, what string) {
	t.Helper()
	st := readSupersedeState(t, s, id)
	if st.by.Valid {
		t.Errorf("%s (id %d) must stay active, superseded_by = %d (reason %q)",
			what, id, st.by.Int64, st.reason.String)
	}
	if st.validUntil.Valid {
		t.Errorf("%s (id %d) must keep valid_until NULL, got %q", what, id, st.validUntil.String)
	}
}

func requireSuperseded(t *testing.T, s *Store, id int64, what string) {
	t.Helper()
	st := readSupersedeState(t, s, id)
	if !st.by.Valid {
		t.Errorf("%s (id %d) must be superseded, but superseded_by is NULL", what, id)
	}
}

// A short session may still carry learnings the user stated themselves: they are
// written mid-session by remember() and the hooks, long before the session has
// enough messages for extraction. Session length may never discard them.
func TestSupersedeShortSessionLearnings_PreservesUserStatedAndAgreedUpon(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-short", 3, 72*time.Hour)
	userStated := insertSessionLearning(t, s, "sess-short", "user_stated", "Never mock the database in migration tests")
	agreed := insertSessionLearning(t, s, "sess-short", "agreed_upon", "Deploys happen only through make deploy")
	overridden := insertSessionLearning(t, s, "sess-short", "user_override", "Persona trait the user forced")
	auto := insertSessionLearning(t, s, "sess-short", "llm_extracted", "Vendor X is used for embeddings")
	suggested := insertSessionLearning(t, s, "sess-short", "claude_suggested", "Consider batching the embeds")

	// A long session is never a cleanup candidate, whatever its sources.
	seedShortSession(t, s, "sess-long", 20, 72*time.Hour)
	longSessionLearning := insertSessionLearning(t, s, "sess-long", "llm_extracted", "Long session fact")

	if _, err := s.SupersedeShortSessionLearnings(); err != nil {
		t.Fatalf("SupersedeShortSessionLearnings: %v", err)
	}

	requireActive(t, s, userStated, "user_stated learning of a short session")
	requireActive(t, s, agreed, "agreed_upon learning of a short session")
	requireActive(t, s, overridden, "user_override learning of a short session")
	requireActive(t, s, longSessionLearning, "learning of a long session")
	requireSuperseded(t, s, auto, "llm_extracted learning of a finished short session")
	requireSuperseded(t, s, suggested, "claude_suggested learning of a finished short session")

	// The audit key is a contract with the finder, the restore command and the
	// documented SQL query. Pin the literal, not just the constant.
	if got := readSupersedeState(t, s, auto).reason.String; got != "session too short for extraction" {
		t.Errorf("cleanup wrote supersede_reason %q, want the audit key %q",
			got, "session too short for extraction")
	}
}

// A session that is short at the moment of the check may still grow. Only a
// session that has gone quiet counts as finished.
func TestSupersedeShortSessionLearnings_LeavesRunningSessionsAlone(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-live", 2, 2*time.Minute)
	live := insertSessionLearning(t, s, "sess-live", "llm_extracted", "First insight of a session that is still going")

	seedShortSession(t, s, "sess-finished", 2, 72*time.Hour)
	finished := insertSessionLearning(t, s, "sess-finished", "llm_extracted", "First insight of an abandoned session")

	if _, err := s.SupersedeShortSessionLearnings(); err != nil {
		t.Fatalf("SupersedeShortSessionLearnings: %v", err)
	}

	requireActive(t, s, live, "learning of a short session with recent activity")
	requireSuperseded(t, s, finished, "learning of a finished short session")
}

// Without an end timestamp a session cannot be proven finished, so it stays out.
func TestSupersedeShortSessionLearnings_SkipsSessionsWithoutEndTimestamp(t *testing.T) {
	s := mustOpen(t)

	if err := s.UpsertSession(&models.Session{
		ID: "sess-noend", Project: "/test", ProjectShort: "test",
		MessageCount: 1,
		StartedAt:    time.Now().Add(-48 * time.Hour),
		JSONLPath:    "/sess-noend.jsonl",
		IndexedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	id := insertSessionLearning(t, s, "sess-noend", "llm_extracted", "Insight from a session with an unknown end")

	if _, err := s.SupersedeShortSessionLearnings(); err != nil {
		t.Fatalf("SupersedeShortSessionLearnings: %v", err)
	}

	requireActive(t, s, id, "learning of a session without ended_at")
}

// insertShortSessionNarrative stores a narrative the way the extraction
// pipeline does: auto-extracted, bound to the session it describes.
func insertShortSessionNarrative(t *testing.T, s *Store, sessionID, content string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category: "narrative", Content: content, SessionID: sessionID,
		Source: "llm_extracted", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})
	if err != nil {
		t.Fatalf("insert narrative for %s: %v", sessionID, err)
	}
	return id
}

// The persona synthesis supersedes narratives of short sessions on every run.
// A session that is short only at the moment of the check may still grow, and
// its narrative then describes a session that is still being written.
func TestSupersedeShortNarratives_LeavesRunningSessionsAlone(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-narr-live", 3, time.Hour)
	live := insertShortSessionNarrative(t, s, "sess-narr-live", "Narrative of a session that is still going")

	seedShortSession(t, s, "sess-narr-finished", 3, 72*time.Hour)
	finished := insertShortSessionNarrative(t, s, "sess-narr-finished", "Narrative of an abandoned short session")

	seedShortSession(t, s, "sess-narr-long", 20, 72*time.Hour)
	long := insertShortSessionNarrative(t, s, "sess-narr-long", "Narrative of a finished long session")

	cleaned, err := s.SupersedeShortNarratives()
	if err != nil {
		t.Fatalf("SupersedeShortNarratives: %v", err)
	}
	if len(cleaned) != 1 {
		t.Errorf("cleaned %d narratives, want 1 (only the finished short session)", len(cleaned))
	}

	requireActive(t, s, live, "narrative of a short session with recent activity")
	requireSuperseded(t, s, finished, "narrative of a finished short session")
	requireActive(t, s, long, "narrative of a long session")

	// Keep the reason literal stable: it is what separates the narrative cleanup
	// from the extraction cleanup in the learnings table.
	if got := readSupersedeState(t, s, finished).reason.String; got != "session too short" {
		t.Errorf("narrative cleanup wrote supersede_reason %q, want %q", got, "session too short")
	}
}

// Without an end timestamp a session cannot be proven finished, so its
// narrative stays active.
func TestSupersedeShortNarratives_SkipsSessionsWithoutEndTimestamp(t *testing.T) {
	s := mustOpen(t)

	if err := s.UpsertSession(&models.Session{
		ID: "sess-narr-noend", Project: "/test", ProjectShort: "test",
		MessageCount: 2,
		StartedAt:    time.Now().Add(-48 * time.Hour),
		JSONLPath:    "/sess-narr-noend.jsonl",
		IndexedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	id := insertShortSessionNarrative(t, s, "sess-narr-noend", "Narrative of a session with an unknown end")

	if _, err := s.SupersedeShortNarratives(); err != nil {
		t.Fatalf("SupersedeShortNarratives: %v", err)
	}

	requireActive(t, s, id, "narrative of a session without ended_at")
}

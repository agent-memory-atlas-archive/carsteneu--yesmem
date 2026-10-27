package storage

import (
	"database/sql"
	"testing"
	"time"
)

// A session that narrative generation accepts must keep its narrative. Generation
// needs ShortSessionMaxMessages messages, so a cleanup that cuts higher discards
// the narrative of a session the generator would write again on the next run.
func TestSupersedeShortNarratives_KeepsNarrativesOfExtractableSessions(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-narr-extractable", ShortSessionMaxMessages, 72*time.Hour)
	id := insertShortSessionNarrative(t, s, "sess-narr-extractable", "Narrative of a session long enough to extract")

	if _, err := s.SupersedeShortNarratives(); err != nil {
		t.Fatalf("SupersedeShortNarratives: %v", err)
	}

	requireActive(t, s, id, "narrative of a session at the extraction threshold")
}

// A superseded narrative must not leave its marker behind. narrative_at is what
// the generator reads to skip a session, so a session whose narrative was removed
// would never be narrated again, however long it grew.
func TestSupersedeShortNarratives_ClearsNarrativeMarker(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-narr-marker", 2, 72*time.Hour)
	if err := s.MarkSessionNarrative("sess-narr-marker"); err != nil {
		t.Fatalf("mark session narrative: %v", err)
	}
	id := insertShortSessionNarrative(t, s, "sess-narr-marker", "Narrative of an abandoned short session")

	if _, err := s.SupersedeShortNarratives(); err != nil {
		t.Fatalf("SupersedeShortNarratives: %v", err)
	}
	requireSuperseded(t, s, id, "narrative of a finished short session")

	var marker sql.NullString
	if err := s.DB().QueryRow(`SELECT narrative_at FROM sessions WHERE id = ?`,
		"sess-narr-marker").Scan(&marker); err != nil {
		t.Fatalf("read narrative_at: %v", err)
	}
	if marker.Valid {
		t.Errorf("narrative_at of a session whose narrative was superseded must be NULL, got %q", marker.String)
	}
}

// The Go predicate and the SQL predicate must draw the finished line at the same
// place: one decides the extraction slack, the other the short-session cleanup.
// A session one minute inside the quiet period is still running for both.
func TestShortSessionFinishedPredicateAgreesAcrossGoAndSQL(t *testing.T) {
	now := time.Now()
	running := now.Add(-ShortSessionQuietPeriod + time.Minute)
	finished := now.Add(-ShortSessionQuietPeriod - time.Minute)

	if SessionFinished(running, now) {
		t.Error("a session one minute inside the quiet period is still running")
	}
	if !SessionFinished(finished, now) {
		t.Error("a session one minute past the quiet period is finished")
	}
	if SessionFinished(time.Time{}, now) {
		t.Error("a session without an end timestamp is never proven finished")
	}

	s := mustOpen(t)
	seedShortSession(t, s, "sess-pred-running", 2, ShortSessionQuietPeriod-time.Minute)
	runningLearning := insertSessionLearning(t, s, "sess-pred-running", "llm_extracted",
		"Insight from a session that is still running")
	seedShortSession(t, s, "sess-pred-finished", 2, ShortSessionQuietPeriod+time.Minute)
	finishedLearning := insertSessionLearning(t, s, "sess-pred-finished", "llm_extracted",
		"Insight from a session that has gone quiet")

	if _, err := s.SupersedeShortSessionLearnings(); err != nil {
		t.Fatalf("SupersedeShortSessionLearnings: %v", err)
	}

	requireActive(t, s, runningLearning, "learning of a session inside the quiet period")
	requireSuperseded(t, s, finishedLearning, "learning of a session past the quiet period")
}

// legacyShortNarrativeReason is the reason string the narrative cleanup has always
// written. The reset keys on it, so the literal is a contract, not an internal name.
const legacyShortNarrativeReason = "session too short"

// markLegacyShortNarrative reproduces a row the cleanup superseded before the
// marker reset existed.
func markLegacyShortNarrative(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE learnings
		SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now')
		WHERE id = ?`, legacyShortNarrativeReason, id); err != nil {
		t.Fatalf("mark legacy short narrative %d: %v", id, err)
	}
}

func readNarrativeMarker(t *testing.T, s *Store, sessionID string) sql.NullString {
	t.Helper()
	var marker sql.NullString
	if err := s.DB().QueryRow(`SELECT narrative_at FROM sessions WHERE id = ?`, sessionID).Scan(&marker); err != nil {
		t.Fatalf("read narrative_at of %s: %v", sessionID, err)
	}
	return marker
}

// A session whose short narrative was discarded before the reset existed and that
// has grown past the threshold since keeps a stale marker: the cleanup no longer
// matches it, so only a reset that runs on its own can repair it.
func TestSupersedeShortNarratives_RepairsLegacyMarkerWithoutNewSupersedes(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-legacy-marker", ShortSessionMaxMessages+2, 72*time.Hour)
	if err := s.MarkSessionNarrative("sess-legacy-marker"); err != nil {
		t.Fatalf("mark session narrative: %v", err)
	}
	legacy := insertShortSessionNarrative(t, s, "sess-legacy-marker",
		"Narrative of a session that was too short when it was written")
	markLegacyShortNarrative(t, s, legacy)

	cleaned, err := s.SupersedeShortNarratives()
	if err != nil {
		t.Fatalf("SupersedeShortNarratives: %v", err)
	}
	if len(cleaned) != 0 {
		t.Fatalf("superseded %d narratives, want 0: the session grew past the threshold", len(cleaned))
	}

	if marker := readNarrativeMarker(t, s, "sess-legacy-marker"); marker.Valid {
		t.Errorf("stale narrative_at of a session without an active narrative must be repaired, got %q", marker.String)
	}
}

// A session that carries a live narrative keeps its marker. Clearing it sends the
// session back to the generator, which writes a second story instead of one.
func TestSupersedeShortNarratives_KeepsMarkerOfSessionWithActiveNarrative(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-live-narrative", ShortSessionMaxMessages+2, 72*time.Hour)
	if err := s.MarkSessionNarrative("sess-live-narrative"); err != nil {
		t.Fatalf("mark session narrative: %v", err)
	}
	old := insertShortSessionNarrative(t, s, "sess-live-narrative", "Superseded narrative of an earlier run")
	markLegacyShortNarrative(t, s, old)
	live := insertShortSessionNarrative(t, s, "sess-live-narrative", "Narrative the session carries today")

	// A finished short session gives this run something to supersede, so the
	// cleanup does not return early and the reset really executes.
	seedShortSession(t, s, "sess-fresh-short", 2, 72*time.Hour)
	fresh := insertShortSessionNarrative(t, s, "sess-fresh-short", "Narrative of an abandoned short session")

	cleaned, err := s.SupersedeShortNarratives()
	if err != nil {
		t.Fatalf("SupersedeShortNarratives: %v", err)
	}
	if len(cleaned) != 1 {
		t.Fatalf("superseded %d narratives, want 1 (only the finished short session)", len(cleaned))
	}
	requireSuperseded(t, s, fresh, "narrative of the finished short session")

	requireActive(t, s, live, "live narrative of a session past the threshold")
	if marker := readNarrativeMarker(t, s, "sess-live-narrative"); !marker.Valid {
		t.Error("narrative_at of a session with a live narrative must stay set")
	}
}

// The daemon cleanup supersedes every auto-extracted row of a finished short
// session, narrative rows included, and writes the extraction reason. The reset
// must cover that path too, or a short session keeps its marker and never gets a
// narrative again once it grows.
func TestSupersedeShortNarratives_RepairsMarkerClearedByTheExtractionCleanup(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-extraction-reason", ShortSessionMaxMessages+2, 72*time.Hour)
	if err := s.MarkSessionNarrative("sess-extraction-reason"); err != nil {
		t.Fatalf("mark session narrative: %v", err)
	}
	cleaned := insertShortSessionNarrative(t, s, "sess-extraction-reason",
		"Narrative of a session that was too short when it was written")
	if _, err := s.DB().Exec(`UPDATE learnings
		SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now')
		WHERE id = ?`, ShortSessionSupersedeReason, cleaned); err != nil {
		t.Fatalf("supersede with the extraction reason: %v", err)
	}

	if _, err := s.SupersedeShortNarratives(); err != nil {
		t.Fatalf("SupersedeShortNarratives: %v", err)
	}

	if marker := readNarrativeMarker(t, s, "sess-extraction-reason"); marker.Valid {
		t.Errorf("narrative_at of a session whose narrative the extraction cleanup removed must be cleared, got %q", marker.String)
	}
}

// Whatever removed a narrative for another reason stays out of reach: the user
// cleaned some narratives by hand, and handing those sessions back to the
// generator recreates exactly the text that was removed.
func TestSupersedeShortNarratives_SparesMarkerOfForeignSupersedeReason(t *testing.T) {
	s := mustOpen(t)

	seedShortSession(t, s, "sess-user-cleanup", ShortSessionMaxMessages+2, 72*time.Hour)
	if err := s.MarkSessionNarrative("sess-user-cleanup"); err != nil {
		t.Fatalf("mark session narrative: %v", err)
	}
	id := insertShortSessionNarrative(t, s, "sess-user-cleanup", "Narrative the user removed by hand")
	if _, err := s.DB().Exec(`UPDATE learnings
		SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now')
		WHERE id = ?`, "Marked 2026-08-24 (loop/perf-wave attractor)", id); err != nil {
		t.Fatalf("supersede by hand: %v", err)
	}

	if _, err := s.SupersedeShortNarratives(); err != nil {
		t.Fatalf("SupersedeShortNarratives: %v", err)
	}

	if marker := readNarrativeMarker(t, s, "sess-user-cleanup"); !marker.Valid {
		t.Error("a narrative superseded for a foreign reason must not clear the marker")
	}
}

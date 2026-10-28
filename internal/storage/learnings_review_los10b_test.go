package storage

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// TestGetLearningsSinceComparesParsedTimestampsLos10b (YM-246 [MEM-F29]): the
// metamemory path compared created_at strings. A row written with a zone ahead
// of the bound sorts after the bound lexicographically while its instant is
// before it, so an out-of-window learning entered the prompt.
func TestGetLearningsSinceComparesParsedTimestampsLos10b(t *testing.T) {
	s := mustOpen(t)

	// 45 minutes old, written in +03:00: its wall clock is 45 minutes AHEAD of a
	// bound taken 30 minutes ago in +02:00, its instant is 15 minutes behind it.
	rowCreated := time.Now().Add(-45 * time.Minute).In(time.FixedZone("LOS10B-EEST", 3*3600))
	bound := time.Now().Add(-30 * time.Minute).In(los10bOffset)

	id := insertTimedLos10bLearning(t, s, "los10b metamemory sentinel", rowCreated)

	// Control: a bound three hours back keeps the row.
	control, err := s.GetLearningsSince("los10b-proj", time.Now().Add(-3*time.Hour), 10)
	if err != nil {
		t.Fatalf("get learnings since (control): %v", err)
	}
	found := false
	for _, l := range control {
		if l.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("control failed: learning %d missing from a three hour window", id)
	}

	out, err := s.GetLearningsSince("los10b-proj", bound, 10)
	if err != nil {
		t.Fatalf("get learnings since: %v", err)
	}
	for _, l := range out {
		if l.ID == id {
			t.Errorf("learning created %s is older than since=%s but was returned",
				rowCreated.Format(time.RFC3339), bound.Format(time.RFC3339))
		}
	}
}

// TestSearchLearningsBM25CtxExcludesSupersededLos10b (YM-245 [MEM-F28]): the BM25
// metadata gate checked quarantine only, while the FTS index keeps a superseded
// row. SearchLearningsBM25 is exported and consumed without a supersede
// resolution, so the retired row leaked out of it.
func TestSearchLearningsBM25CtxExcludesSupersededLos10b(t *testing.T) {
	s := mustOpen(t)

	oldID := insertTestLearning(s, "los10b superseded sentinel alpha", "gotcha")
	newID := insertTestLearning(s, "los10b replacement sentinel beta", "gotcha")
	if err := s.SupersedeLearning(oldID, newID, "replaced"); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	hits, err := s.SearchLearningsBM25Ctx(context.Background(), "los10b superseded sentinel alpha", "", "", "", 5)
	if err != nil {
		t.Fatalf("bm25: %v", err)
	}
	for _, h := range hits {
		if h.ID == strconv.FormatInt(oldID, 10) {
			t.Errorf("superseded learning %d was returned by the BM25 search", oldID)
		}
	}
}

// TestQuarantineSessionKeepsCapsLos10b (YM-245 [MEM-F28]): crash recovery
// quarantines a failed agent's whole session. A cap is tooling, not recall
// content, so quarantining must not make it silently unexecutable.
func TestQuarantineSessionKeepsCapsLos10b(t *testing.T) {
	s := mustOpen(t)

	const session = "sess-los10b-cap"
	capID, err := s.InsertLearning(&models.Learning{
		SessionID:  session,
		Category:   "cap",
		Content:    "cap los10b_probe",
		Project:    "los10b-proj",
		Context:    `{"name":"los10b_probe"}`,
		Confidence: 1.0,
		CreatedAt:  time.Now(),
		ModelUsed:  "test",
		Source:     "user_stated",
	})
	if err != nil {
		t.Fatalf("insert cap: %v", err)
	}
	plainID := insertQuarantineLos10bLearning(t, s, "crashed session guess", session, "los10b-proj", "")

	affected, err := s.QuarantineSession(session)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if affected != 1 {
		t.Fatalf("quarantine touched %d rows, want 1 (the non-cap row)", affected)
	}

	caps, err := s.GetActiveLearnings("cap", "los10b-proj", "", "", 0)
	if err != nil {
		t.Fatalf("list caps: %v", err)
	}
	capVisible := false
	for _, l := range caps {
		if l.ID == capID {
			capVisible = true
		}
	}
	if !capVisible {
		t.Errorf("cap %d became invisible after its session was quarantined", capID)
	}

	learnings, err := s.GetActiveLearnings("gotcha", "los10b-proj", "", "", 0)
	if err != nil {
		t.Fatalf("list gotchas: %v", err)
	}
	for _, l := range learnings {
		if l.ID == plainID {
			t.Errorf("non-cap learning %d stayed visible after quarantine", plainID)
		}
	}
}

// TestCountActiveUnfinishedExcludesQuarantinedLos10b (YM-245 [MEM-F28]): the
// briefing counter counted quarantined open items while the list beside it
// (GetActiveLearnings) no longer showed them.
func TestCountActiveUnfinishedExcludesQuarantinedLos10b(t *testing.T) {
	s := mustOpen(t)

	id, err := s.InsertLearning(&models.Learning{
		SessionID:  "sess-los10b-unfinished",
		Category:   "unfinished",
		Content:    "los10b open task",
		Project:    "los10b-proj",
		Confidence: 1.0,
		CreatedAt:  time.Now(),
		ModelUsed:  "test",
		Source:     "llm_extracted",
	})
	if err != nil {
		t.Fatalf("insert unfinished: %v", err)
	}
	if n, err := s.CountActiveUnfinished("los10b-proj"); err != nil || n != 1 {
		t.Fatalf("control failed: count=%d err=%v, want 1", n, err)
	}

	if _, err := s.QuarantineSession("sess-los10b-unfinished"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	n, err := s.CountActiveUnfinished("los10b-proj")
	if err != nil {
		t.Fatalf("count unfinished: %v", err)
	}
	if n != 0 {
		t.Errorf("counter reports %d open items, want 0: learning %d is quarantined", n, id)
	}
}

// TestContentHashDedupIgnoresQuarantinedLos10b (YM-245 [MEM-F28]): the write-side
// dedup gate answered on quarantined rows. Crash recovery quarantines a failed
// agent's session, the retry re-extracts the same text, and the dedup then
// matched the quarantined row: the save was reported as a duplicate, nothing was
// inserted, and the content was unreachable from both ends.
func TestContentHashDedupIgnoresQuarantinedLos10b(t *testing.T) {
	s := mustOpen(t)

	const content = "los10b duplicate gate sentinel"
	id := insertQuarantineLos10bLearning(t, s, content, "sess-los10b-dedup", "los10b-proj", "")
	hash := textutil.ContentHash(content)

	found, err := s.GetLearningByContentHash(hash)
	if err != nil {
		t.Fatalf("content hash lookup (control): %v", err)
	}
	if found == nil || found.ID != id {
		t.Fatalf("control failed: the active row was not found by its content hash")
	}

	if _, err := s.QuarantineSession("sess-los10b-dedup"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	// No match is reported as sql.ErrNoRows by this QueryRow helper; both callers
	// treat "err != nil" as "no duplicate", which is the wanted outcome here.
	found, _ = s.GetLearningByContentHash(hash)
	if found != nil {
		t.Errorf("quarantined learning %d answered the dedup gate: the same content can never be saved again", found.ID)
	}
}

// TestCountActiveUnfinishedComparesExpiryAsInstantLos10b (YM-246 [MEM-F29]): the
// counter compared expires_at as text. Deadlines are written as "T00:00:00Z"
// while the bound is local time, so an item expiring in 90 minutes counted as
// already expired and the open-work reminder stayed silent.
func TestCountActiveUnfinishedComparesExpiryAsInstantLos10b(t *testing.T) {
	s := mustOpen(t)

	expires := time.Now().Add(90 * time.Minute).UTC()
	if _, err := s.InsertLearning(&models.Learning{
		SessionID:  "sess-los10b-expiry",
		Category:   "unfinished",
		Content:    "los10b deadline task",
		Project:    "los10b-proj",
		Confidence: 1.0,
		CreatedAt:  time.Now(),
		ExpiresAt:  &expires,
		ModelUsed:  "test",
		Source:     "llm_extracted",
	}); err != nil {
		t.Fatalf("insert unfinished: %v", err)
	}

	n, err := s.CountActiveUnfinished("los10b-proj")
	if err != nil {
		t.Fatalf("count unfinished: %v", err)
	}
	if n != 1 {
		t.Errorf("counter reports %d open items, want 1: the item expires in 90 minutes", n)
	}
}

package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los10cZones returns two locations one hour ahead and one hour behind the local
// zone. Two rows carrying the SAME instant written in these zones produce
// different lexical orderings against any bound, so a string comparison can
// never treat them alike while a parsed comparison must. That makes the RED
// independent of the host zone.
func los10cZones() (ahead, behind *time.Location) {
	return los10cZoneOffset(1), los10cZoneOffset(-1)
}

// los10cZoneOffset returns a location offset by the given hours from the local zone.
func los10cZoneOffset(hours int) *time.Location {
	_, off := time.Now().Zone()
	return time.FixedZone("LOS10C_OFF", off+hours*3600)
}

// insertLos10cLearning inserts one row of the los10c fixture family.
func insertLos10cLearning(t *testing.T, s *Store, content, sessionID string, createdAt time.Time, opts ...func(*models.Learning)) int64 {
	t.Helper()
	l := &models.Learning{
		SessionID:  sessionID,
		Category:   "gotcha",
		Content:    content,
		Confidence: 1.0,
		CreatedAt:  createdAt,
		ModelUsed:  "test",
		Source:     "llm_extracted",
	}
	for _, opt := range opts {
		opt(l)
	}
	id, err := s.InsertLearning(l)
	if err != nil {
		t.Fatalf("insert learning %q: %v", content, err)
	}
	return id
}

func los10cExpiresAt(ts time.Time) func(*models.Learning) {
	return func(l *models.Learning) { l.ExpiresAt = &ts }
}

func los10cTriggerRule(rule string) func(*models.Learning) {
	return func(l *models.Learning) { l.TriggerRule = rule }
}

func los10cFlavor(flavor string) func(*models.Learning) {
	return func(l *models.Learning) { l.SessionFlavor = flavor }
}

func los10cSource(source string) func(*models.Learning) {
	return func(l *models.Learning) { l.Source = source }
}

// expiryFixture writes two live rows (one hour left) and two expired rows
// (one minute ago) whose instants are identical inside each pair but whose
// text carries opposite offsets.
type expiryFixture struct {
	liveIDs   []int64
	expiredID []int64
}

func insertExpiryFixtureLos10c(t *testing.T, s *Store, sessionID string) expiryFixture {
	t.Helper()
	ahead, behind := los10cZones()
	now := time.Now()
	live := now.Add(time.Hour)
	expired := now.Add(-time.Minute)

	liveA := insertLos10cLearning(t, s, "los10c live ahead "+sessionID, sessionID, now.Add(-2*time.Hour), los10cExpiresAt(live.In(ahead)), los10cFlavor("live-ahead"), los10cTriggerRule("deadline:2026-12-01"))
	liveB := insertLos10cLearning(t, s, "los10c live behind "+sessionID, sessionID, now.Add(-2*time.Hour), los10cExpiresAt(live.In(behind)), los10cFlavor("live-behind"), los10cTriggerRule("deadline:2026-12-01"))
	expA := insertLos10cLearning(t, s, "los10c expired ahead "+sessionID, sessionID, now.Add(-2*time.Hour), los10cExpiresAt(expired.In(ahead)), los10cFlavor("expired-ahead"), los10cTriggerRule("deadline:2026-12-01"))
	expB := insertLos10cLearning(t, s, "los10c expired behind "+sessionID, sessionID, now.Add(-2*time.Hour), los10cExpiresAt(expired.In(behind)), los10cFlavor("expired-behind"), los10cTriggerRule("deadline:2026-12-01"))
	return expiryFixture{liveIDs: []int64{liveA, liveB}, expiredID: []int64{expA, expB}}
}

func idsOfLearnings(ls []models.Learning) map[int64]bool {
	out := make(map[int64]bool, len(ls))
	for _, l := range ls {
		out[l.ID] = true
	}
	return out
}

// TestActiveReadPathsHonourExpiryInstantsLos10c (YM-305 [MEM-F37]): every read
// path compared expires_at as text. A row that expired a minute ago but was
// written in a zone an hour ahead looked alive, and a row with an hour left
// written an hour behind looked archived.
func TestActiveReadPathsHonourExpiryInstantsLos10c(t *testing.T) {
	s := mustOpen(t)
	fx := insertExpiryFixtureLos10c(t, s, "sess-los10c-expiry")

	bySession, err := s.GetActiveLearningsBySessionIDs([]string{"sess-los10c-expiry"})
	if err != nil {
		t.Fatalf("GetActiveLearningsBySessionIDs: %v", err)
	}
	assertLos10cReadPath(t, "GetActiveLearningsBySessionIDs", idsOfLearnings(bySession), fx)

	triggered, err := s.GetTriggeredLearnings("", "deadline:", "")
	if err != nil {
		t.Fatalf("GetTriggeredLearnings: %v", err)
	}
	assertLos10cReadPath(t, "GetTriggeredLearnings", idsOfLearnings(triggered), fx)

	pending, err := s.GetPendingLearningsForEmbedding(100)
	if err != nil {
		t.Fatalf("GetPendingLearningsForEmbedding: %v", err)
	}
	assertLos10cReadPath(t, "GetPendingLearningsForEmbedding", idsOfLearnings(pending), fx)

	consolidation, err := s.GetConsolidationLearnings(0, 1<<40)
	if err != nil {
		t.Fatalf("GetConsolidationLearnings: %v", err)
	}
	assertLos10cReadPath(t, "GetConsolidationLearnings", idsOfLearnings(consolidation), fx)

	counts, err := s.GetLearningCounts("")
	if err != nil {
		t.Fatalf("GetLearningCounts: %v", err)
	}
	if counts["gotcha"] != len(fx.liveIDs) {
		t.Errorf("GetLearningCounts: got %d live gotchas, want %d (expired rows must not be counted)", counts["gotcha"], len(fx.liveIDs))
	}
}

// TestSessionFlavorsHonourExpiryInstantsLos10c covers the flavor read path, which
// returns a different shape than the learning read paths.
func TestSessionFlavorsHonourExpiryInstantsLos10c(t *testing.T) {
	s := mustOpen(t)
	insertExpiryFixtureLos10c(t, s, "sess-los10c-flavor")

	flavors, err := s.GetSessionFlavorsForSession("sess-los10c-flavor")
	if err != nil {
		t.Fatalf("GetSessionFlavorsForSession: %v", err)
	}
	got := make(map[string]bool, len(flavors))
	for _, f := range flavors {
		if name, ok := f["session_flavor"].(string); ok {
			got[name] = true
		}
	}
	for _, want := range []string{"live-ahead", "live-behind"} {
		if !got[want] {
			t.Errorf("flavor %q must survive: %v", want, got)
		}
	}
	for _, unwanted := range []string{"expired-ahead", "expired-behind"} {
		if got[unwanted] {
			t.Errorf("flavor %q comes from an expired learning and must be filtered: %v", unwanted, got)
		}
	}
}

func assertLos10cReadPath(t *testing.T, name string, got map[int64]bool, fx expiryFixture) {
	t.Helper()
	for _, id := range fx.liveIDs {
		if !got[id] {
			t.Errorf("%s: live learning %d is missing from the result", name, id)
		}
	}
	for _, id := range fx.expiredID {
		if got[id] {
			t.Errorf("%s: expired learning %d was returned", name, id)
		}
	}
}

// TestStatsFilterHonoursParsedWindowsLos10c (YM-305 [MEM-F37]): StatsFilter built
// its WHERE clause from raw strings, so the same instant written in two offsets
// landed on different sides of the window.
func TestStatsFilterHonoursParsedWindowsLos10c(t *testing.T) {
	s := mustOpen(t)
	ahead, behind := los10cZones()
	now := time.Now()

	// Two hours outside and two hours inside the reference bound, each written in
	// both directions.
	older := now.Add(-3 * time.Hour)
	newer := now.Add(-10 * time.Minute)
	insertLos10cLearning(t, s, "los10c stats older ahead", "sess-los10c-stats", older.In(ahead))
	insertLos10cLearning(t, s, "los10c stats older behind", "sess-los10c-stats", older.In(behind))
	insertLos10cLearning(t, s, "los10c stats newer ahead", "sess-los10c-stats", newer.In(ahead))
	insertLos10cLearning(t, s, "los10c stats newer behind", "sess-los10c-stats", newer.In(behind))

	st, err := s.GetLearningStatsF(StatsFilter{Since: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("GetLearningStatsF since: %v", err)
	}
	if st.ActiveCount != 2 {
		t.Errorf("since window: got %d active learnings, want the 2 rows created ten minutes ago", st.ActiveCount)
	}

	st, err = s.GetLearningStatsF(StatsFilter{Before: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("GetLearningStatsF before: %v", err)
	}
	if st.ActiveCount != 2 {
		t.Errorf("before window: got %d active learnings, want the 2 rows created three hours ago", st.ActiveCount)
	}
}

// TestStatsExpiryCountsUseInstantsLos10c covers the two counts that compare
// expires_at against datetime('now') as text: an expired row written with an
// earlier date-time text counted as active and never as archived.
func TestStatsExpiryCountsUseInstantsLos10c(t *testing.T) {
	s := mustOpen(t)
	ahead, _ := los10cZones()
	now := time.Now()

	insertLos10cLearning(t, s, "los10c stats expired ahead", "sess-los10c-stats-exp", now.Add(-2*time.Hour), los10cExpiresAt(now.Add(-time.Minute).In(ahead)))
	insertLos10cLearning(t, s, "los10c stats live", "sess-los10c-stats-exp", now.Add(-2*time.Hour), los10cExpiresAt(now.Add(time.Hour)))

	st, err := s.GetLearningStatsF(StatsFilter{})
	if err != nil {
		t.Fatalf("GetLearningStatsF: %v", err)
	}
	if st.ActiveCount != 1 {
		t.Errorf("ActiveCount = %d, want 1 (the expired row must not count as active)", st.ActiveCount)
	}
	if st.ArchivedCount != 1 {
		t.Errorf("ArchivedCount = %d, want 1 (the expired row must count as archived)", st.ArchivedCount)
	}
}

// TestAutoCorrectGenerationWindowUsesInstantsLos10c: the per-cap generation
// budget compared created_at as text, so a row two hours old written an hour
// ahead fell inside a one hour window and consumed budget it did not use.
func TestAutoCorrectGenerationWindowUsesInstantsLos10c(t *testing.T) {
	s := mustOpen(t)
	ahead, behind := los10cZones()
	now := time.Now()
	outside := now.Add(-2 * time.Hour)

	insertLos10cLearning(t, s, "los10c cap outside ahead", "sess-los10c-cap", outside.In(ahead), los10cSource("auto_correct_accepted"), los10cTriggerRule("cap:los10c"))
	insertLos10cLearning(t, s, "los10c cap outside behind", "sess-los10c-cap", outside.In(behind), los10cSource("auto_correct_accepted"), los10cTriggerRule("cap:los10c"))

	n, err := s.CountAutoCorrectGenerations("los10c", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CountAutoCorrectGenerations: %v", err)
	}
	if n != 0 {
		t.Errorf("generations = %d, want 0: both rows are two hours old, the window is one hour", n)
	}
}

// TestCountLearningSinceUsesInstantsLos10c (YM-305 [MEM-F37]): the counter compared
// created_at as text, so a two hour old row written an hour ahead satisfied a one
// hour cutoff. No production caller today, but it is the same class of comparison.
func TestCountLearningSinceUsesInstantsLos10c(t *testing.T) {
	s := mustOpen(t)
	ahead, behind := los10cZones()
	now := time.Now()
	outside := now.Add(-2 * time.Hour)

	insertLos10cLearning(t, s, "los10c countsince ahead", "sess-los10c-cs", outside.In(ahead))
	insertLos10cLearning(t, s, "los10c countsince behind", "sess-los10c-cs", outside.In(behind))

	if n := s.CountLearningSince(now.Add(-time.Hour)); n != 0 {
		t.Errorf("CountLearningSince = %d, want 0: both rows are two hours old, the cutoff is one hour", n)
	}
	if n := s.CountLearningSince(now.Add(-3 * time.Hour)); n != 2 {
		t.Errorf("control: CountLearningSince = %d, want 2 from a cutoff three hours back", n)
	}
}

// TestBriefingStaleCountUsesInstantsLos10c: the briefing's stale counter compared
// created_at against datetime('now','-90 days') as text. Because created_at holds
// a 'T' separator where the cutoff holds a space, the text comparison is decided
// by the DATE part; the fixture therefore places a row one hour off the cutoff in
// a zone that pushes its wall clock onto the wrong date. Which direction is used
// depends on the cutoff's time of day, so the case is always constructible.
func TestBriefingStaleCountUsesInstantsLos10c(t *testing.T) {
	s := mustOpen(t)
	now := time.Now().UTC()
	cutoff := now.AddDate(0, 0, -90)
	tod := cutoff.Hour()

	var boundary, control time.Time
	var zone *time.Location
	if tod <= 12 {
		// One hour AFTER the cutoff (not stale), written so its wall clock falls on
		// the previous date: a string compare calls it stale.
		zone = time.FixedZone("LOS10C_BACK", -(tod+2)*3600)
		boundary = cutoff.Add(time.Hour)
		control = now.AddDate(0, 0, -91)
	} else {
		// One hour BEFORE the cutoff (stale), written so its wall clock falls on the
		// next date: a string compare calls it young.
		zone = time.FixedZone("LOS10C_FWD", (25-tod)*3600)
		boundary = cutoff.Add(-time.Hour)
		control = now.Add(-89 * 24 * time.Hour)
	}

	insertLos10cLearning(t, s, "los10c briefing boundary", "sess-los10c-brief", boundary.In(zone))
	insertLos10cLearning(t, s, "los10c briefing control", "sess-los10c-brief", control.UTC())

	h, err := s.GetBriefingHealth("")
	if err != nil {
		t.Fatalf("GetBriefingHealth: %v", err)
	}
	if h.Stale != 1 {
		t.Errorf("Stale = %d, want 1: exactly one of the two rows is on the stale side of the cutoff", h.Stale)
	}
}

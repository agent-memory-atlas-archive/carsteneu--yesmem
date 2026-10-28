package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/models"
)

// los19StubExtractor satisfies SessionExtractor without calling an LLM.
type los19StubExtractor struct{}

func (los19StubExtractor) ExtractFromSession(string, []models.Message) ([]models.Learning, error) {
	return nil, nil
}

func (los19StubExtractor) ExtractAndStore(string, string, []models.Message, bool) error { return nil }

func (los19StubExtractor) SetChunkSize(int) {}

// YM-49: the profile freshness bar was MIN(sessionCount, 20), so a profile
// generated from the first 20 sessions of a project counted as current forever
// and never picked up the sessions after them.
func TestLos19_CountPendingProfilesUnfreezesAfterTwentySessions(t *testing.T) {
	s := mustStore(t)
	for i := 0; i < 25; i++ {
		if err := s.UpsertSession(&models.Session{
			ID:           fmt.Sprintf("los19-prof-%02d", i),
			Project:      "/work/los19prof",
			ProjectShort: "los19prof",
			StartedAt:    time.Now().Add(-time.Duration(i) * time.Hour),
			JSONLPath:    "/nonexistent.jsonl",
			IndexedAt:    time.Now(),
			MessageCount: 10,
		}); err != nil {
			t.Fatalf("upsert session %d: %v", i, err)
		}
	}
	if err := s.UpsertProjectProfile(&models.ProjectProfile{
		Project:      "/work/los19prof",
		ProfileText:  "stale profile text",
		GeneratedAt:  time.Now().Add(-30 * 24 * time.Hour),
		UpdatedAt:    time.Now().Add(-30 * 24 * time.Hour),
		SessionCount: 20,
		ModelUsed:    "haiku",
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	pending, err := countPendingProfiles(s)
	if err != nil {
		t.Fatalf("countPendingProfiles: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending profiles = %d, want 1: the profile covers 20 of 25 sessions and must be regenerated", pending)
	}
}

// YM-51: a fork only covers the messages that existed when it ran, but the fork
// handler marked the whole session as extracted. The tail of long sessions never
// reached the batch pass.
func TestLos19_BatchExtractionReopensSessionWithUncoveredTail(t *testing.T) {
	s := mustStore(t)
	sess := &models.Session{
		ID:           "los19-fork-long",
		Project:      "/work/los19fork",
		ProjectShort: "los19fork",
		StartedAt:    time.Now().Add(-48 * time.Hour),
		EndedAt:      time.Now().Add(-time.Minute),
		JSONLPath:    "/nonexistent.jsonl",
		IndexedAt:    time.Now(),
		MessageCount: 40,
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := s.MarkSessionExtracted(sess.ID); err != nil {
		t.Fatalf("mark extracted: %v", err)
	}
	if err := s.InsertForkCoverage(sess.ID, 0, 19, 1); err != nil {
		t.Fatalf("insert fork coverage: %v", err)
	}

	stored, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !batchExtractionNeeded(s, *stored) {
		t.Fatalf("session whose forks stop at message 19 of 40 (20 uncovered) must be scheduled for batch extraction")
	}

	// A full pass certifies the range it saw; afterwards the session closes again.
	if err := s.InsertForkCoverage(sess.ID, 0, 39, 0); err != nil {
		t.Fatalf("insert batch coverage: %v", err)
	}
	if batchExtractionNeeded(s, *stored) {
		t.Fatalf("a session covered up to its last message must not be re-extracted")
	}
}

// YM-51: a session that is still growing must not be re-extracted in full on
// every cycle. Only a tail of at least forkCoverageSlack messages reopens it.
func TestLos19_BatchExtractionToleratesAShortTail(t *testing.T) {
	s := mustStore(t)
	sess := &models.Session{
		ID:           "los19-fork-slack",
		Project:      "/work/los19slack",
		ProjectShort: "los19slack",
		StartedAt:    time.Now().Add(-48 * time.Hour),
		EndedAt:      time.Now().Add(-time.Minute), // still growing
		JSONLPath:    "/nonexistent.jsonl",
		IndexedAt:    time.Now(),
		MessageCount: 40,
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := s.MarkSessionExtracted(sess.ID); err != nil {
		t.Fatalf("mark extracted: %v", err)
	}
	if err := s.InsertForkCoverage(sess.ID, 0, 20, 1); err != nil { // 19 uncovered
		t.Fatalf("insert fork coverage: %v", err)
	}

	stored, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if batchExtractionNeeded(s, *stored) {
		t.Fatalf("a tail of 19 messages below the slack must not reopen a growing session")
	}
}

// YM-51: a session that went quiet keeps nothing back, however short the tail is.
func TestLos19_BatchExtractionExtractsTheTailOfAFinishedSession(t *testing.T) {
	s := mustStore(t)
	sess := &models.Session{
		ID:           "los19-fork-quiet",
		Project:      "/work/los19quiet",
		ProjectShort: "los19quiet",
		StartedAt:    time.Now().Add(-72 * time.Hour),
		EndedAt:      time.Now().Add(-48 * time.Hour), // silent for two days
		JSONLPath:    "/nonexistent.jsonl",
		IndexedAt:    time.Now(),
		MessageCount: 45,
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := s.MarkSessionExtracted(sess.ID); err != nil {
		t.Fatalf("mark extracted: %v", err)
	}
	if err := s.InsertForkCoverage(sess.ID, 0, 39, 1); err != nil { // 5 uncovered
		t.Fatalf("insert fork coverage: %v", err)
	}

	stored, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !batchExtractionNeeded(s, *stored) {
		t.Fatalf("a finished session with 5 uncovered messages must be extracted")
	}
}

// YM-51: an extracted session can carry an uncovered tail without any
// extracted_at gap, so the batch cycle needs its own signal to run at all.
func TestLos19_CountUncoveredTailsSeesForkCoveredSessions(t *testing.T) {
	s := mustStore(t)
	sess := &models.Session{
		ID:           "los19-fork-count",
		Project:      "/work/los19count",
		ProjectShort: "los19count",
		StartedAt:    time.Now().Add(-48 * time.Hour),
		EndedAt:      time.Now().Add(-time.Minute),
		JSONLPath:    "/nonexistent.jsonl",
		IndexedAt:    time.Now(),
		MessageCount: 40,
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := s.MarkSessionExtracted(sess.ID); err != nil {
		t.Fatalf("mark extracted: %v", err)
	}
	if err := s.InsertForkCoverage(sess.ID, 0, 19, 1); err != nil {
		t.Fatalf("insert fork coverage: %v", err)
	}

	tails, err := s.CountUncoveredTails(forkCoverageSlack)
	if err != nil {
		t.Fatalf("CountUncoveredTails: %v", err)
	}
	if tails != 1 {
		t.Fatalf("uncovered tails = %d, want 1", tails)
	}
}

// YM-49: subagent sessions must not count as project work, otherwise a profile
// looks stale forever once any agent ran in the project.
func TestLos19_CountPendingProfilesIgnoresSubagents(t *testing.T) {
	s := mustStore(t)
	for i := 0; i < 25; i++ {
		if err := s.UpsertSession(&models.Session{
			ID:           fmt.Sprintf("los19-sub-top-%02d", i),
			Project:      "/work/los19sub",
			ProjectShort: "los19sub",
			StartedAt:    time.Now().Add(-time.Duration(i) * time.Hour),
			JSONLPath:    "/nonexistent.jsonl",
			IndexedAt:    time.Now(),
			MessageCount: 10,
		}); err != nil {
			t.Fatalf("upsert session %d: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := s.UpsertSession(&models.Session{
			ID:              fmt.Sprintf("los19-sub-agent-%02d", i),
			Project:         "/work/los19sub",
			ProjectShort:    "los19sub",
			ParentSessionID: "los19-sub-top-00",
			AgentType:       "task",
			StartedAt:       time.Now().Add(-time.Duration(i) * time.Minute),
			JSONLPath:       "/nonexistent.jsonl",
			IndexedAt:       time.Now(),
			MessageCount:    8,
		}); err != nil {
			t.Fatalf("upsert subagent session %d: %v", i, err)
		}
	}
	if err := s.UpsertProjectProfile(&models.ProjectProfile{
		Project:      "/work/los19sub",
		ProfileText:  "current profile text",
		GeneratedAt:  time.Now(),
		UpdatedAt:    time.Now(),
		SessionCount: 25, // all top-level sessions, none of the subagents
		ModelUsed:    "haiku",
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}

	pending, err := countPendingProfiles(s)
	if err != nil {
		t.Fatalf("countPendingProfiles: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending profiles = %d, want 0: subagent sessions must not make the profile look stale", pending)
	}
}

// YM-51: sessions without any fork coverage row were never fork-extracted
// (daemon-internal ones, for instance). They must stay closed, otherwise every
// cycle re-extracts them.
func TestLos19_BatchExtractionLeavesSessionsWithoutForkCoverageAlone(t *testing.T) {
	s := mustStore(t)
	sess := &models.Session{
		ID:           "los19-no-fork",
		Project:      "/work/los19nofork",
		ProjectShort: "los19nofork",
		StartedAt:    time.Now().Add(-48 * time.Hour),
		JSONLPath:    "/nonexistent.jsonl",
		IndexedAt:    time.Now(),
		MessageCount: 40,
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := s.MarkSessionExtracted(sess.ID); err != nil {
		t.Fatalf("mark extracted: %v", err)
	}

	stored, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if batchExtractionNeeded(s, *stored) {
		t.Fatalf("an extracted session without fork coverage must stay closed")
	}
}

// YM-51: the full pass must certify the message range it saw, otherwise the
// session reopens on every cycle.
func TestLos19_RunExtractionPhaseCertifiesTheRangeItSaw(t *testing.T) {
	s := mustStore(t)
	sess := &models.Session{
		ID:           "los19-certify",
		Project:      "/work/los19certify",
		ProjectShort: "los19certify",
		StartedAt:    time.Now().Add(-48 * time.Hour),
		JSONLPath:    "/nonexistent.jsonl",
		IndexedAt:    time.Now(),
		MessageCount: 40,
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	stored, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	RunExtractionPhase(los19StubExtractor{}, s, []models.Session{*stored}, &config.Config{}, nil)

	if s.ForkCoverageIncomplete(sess.ID, 40, 1) {
		t.Fatalf("RunExtractionPhase did not certify the range it saw: session still reports an uncovered tail")
	}
}

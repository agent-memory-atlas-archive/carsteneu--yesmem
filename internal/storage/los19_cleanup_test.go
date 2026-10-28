package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-228: the three content markers sat outside the parenthesized junk
// predicate. AND binds tighter than OR, so the clause selected every row that
// carried a marker, no matter whether it was already superseded. The cleanup
// then overwrote superseded_by with its own sentinel and destroyed the chain.
func TestLos19_CleanupJunkLeavesSupersededRowsAlone(t *testing.T) {
	s := mustOpen(t)

	junkID, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "GEFIXT: cache flush ordering is stable",
		Confidence: 1.0, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert junk learning: %v", err)
	}
	winnerID, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "the current statement of the fact",
		Confidence: 1.0, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert winner learning: %v", err)
	}
	loserID, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "RESOLVED: the older statement of the fact",
		Confidence: 1.0, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert loser learning: %v", err)
	}
	if err := s.SupersedeLearning(loserID, winnerID, "replaced by a newer statement"); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	if _, err := s.CleanupJunkLearnings(); err != nil {
		t.Fatalf("CleanupJunkLearnings: %v", err)
	}

	junk, err := s.GetLearning(junkID)
	if err != nil {
		t.Fatalf("get junk learning: %v", err)
	}
	if junk.SupersededBy == nil || *junk.SupersededBy != -1 {
		t.Fatalf("active junk row not superseded by the cleanup: superseded_by=%v", junk.SupersededBy)
	}

	loser, err := s.GetLearning(loserID)
	if err != nil {
		t.Fatalf("get superseded learning: %v", err)
	}
	if loser.SupersededBy == nil || *loser.SupersededBy != winnerID {
		t.Fatalf("cleanup rewrote an already superseded row: superseded_by=%v, want %d", loser.SupersededBy, winnerID)
	}
}

// YM-51/security: coverage is a trust signal that suppresses extraction. A
// caller must not be able to claim more messages than the session has.
func TestLos19_InsertForkCoverageClampsToTheSessionLength(t *testing.T) {
	s := mustOpen(t)
	if err := s.UpsertSession(&models.Session{
		ID: "los19-clamp", Project: "/work/los19clamp", ProjectShort: "/work/los19clamp",
		StartedAt: time.Now(), JSONLPath: "/nonexistent.jsonl", IndexedAt: time.Now(),
		MessageCount: 30,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	if err := s.InsertForkCoverage("los19-clamp", 0, 500, 1); err != nil {
		t.Fatalf("InsertForkCoverage: %v", err)
	}
	if !s.IsCoveredByFork("los19-clamp", 0, 29) {
		t.Fatalf("coverage was not clamped to the session length: messages 0..29 not covered")
	}
	if s.IsCoveredByFork("los19-clamp", 0, 30) {
		t.Fatalf("coverage claims message 30, which the session does not have")
	}

	if err := s.InsertForkCoverage("los19-clamp", -1, 5, 2); err == nil {
		t.Fatalf("negative from_idx accepted")
	}
	if err := s.InsertForkCoverage("los19-clamp", 10, 5, 2); err == nil {
		t.Fatalf("inverted range accepted")
	}
}

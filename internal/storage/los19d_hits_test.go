package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-362 review finding I-2: after SupersedeSessionLearnings spares the
// user-owned rows, MigrateHitCounts still distributes the counters of the
// superseded machine rows onto them as well. The spared row would collect
// relevance it never earned, and the replacement row would share it.
func TestLos19d_MigrateHitCountsSkipsUserOwnedRows(t *testing.T) {
	s := mustOpen(t)
	const sid = "sess-los19d-hitcounts"

	oldID, err := s.InsertLearning(&models.Learning{
		SessionID: sid, Category: "gotcha", Content: "Alter maschineller Befund mit drei Treffern",
		Source: "llm_extracted", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "haiku",
	})
	if err != nil {
		t.Fatalf("insert old learning: %v", err)
	}
	for i := 0; i < 3; i++ {
		s.IncrementHitCounts([]int64{oldID})
	}

	userID, err := s.InsertLearning(&models.Learning{
		SessionID: sid, Category: "gotcha", Content: "Nutzerzeile, die der Reextract schont",
		Source: "agreed_upon", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})
	if err != nil {
		t.Fatalf("insert user learning: %v", err)
	}

	if _, err := s.SupersedeSessionLearnings(sid); err != nil {
		t.Fatalf("supersede session learnings: %v", err)
	}

	newID, err := s.InsertLearning(&models.Learning{
		SessionID: sid, Category: "gotcha", Content: "Neuer maschineller Befund nach dem Reextract",
		Source: "llm_extracted", Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "haiku",
	})
	if err != nil {
		t.Fatalf("insert new learning: %v", err)
	}

	if _, err := s.MigrateHitCounts(); err != nil {
		t.Fatalf("migrate hit counts: %v", err)
	}

	user, err := s.GetLearning(userID)
	if err != nil {
		t.Fatalf("get user learning: %v", err)
	}
	if user.HitCount != 0 {
		t.Errorf("spared user row received hit_count %d, want 0", user.HitCount)
	}
	replacement, err := s.GetLearning(newID)
	if err != nil {
		t.Fatalf("get new learning: %v", err)
	}
	if replacement.HitCount != 3 {
		t.Errorf("replacement row received hit_count %d, want the 3 hits of the superseded row", replacement.HitCount)
	}
}

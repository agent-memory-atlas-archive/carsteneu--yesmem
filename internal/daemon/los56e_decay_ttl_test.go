package daemon

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/embedding"
)

// TestLos56eDecayAppliesToBoundedTTL covers the review finding on YM-348: the
// decay lookup filtered with valid_until IS NULL, so a bounded volatile learning
// (valid_until set, still active) fell out of the map and kept its full RRF
// weight while its peers decayed. That was invisible only because the store open
// wiped every TTL.
func TestLos56eDecayAppliesToBoundedTTL(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().Format("2006-01-02 15:04:05")
	future := time.Now().Add(30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := s.DB().Exec(`INSERT INTO learnings(id, content, category, project, created_at, model_used, source, turns_at_creation, valid_until)
		VALUES (1, 'bounded pattern', 'pattern', 'memory', ?, 'test', 'llm_extracted', 0, ?)`, now, future); err != nil {
		t.Fatalf("insert bounded learning: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT OR REPLACE INTO turn_counters(project, turn_count) VALUES ('memory', 100)`); err != nil {
		t.Fatalf("seed turn counter: %v", err)
	}

	results := []embedding.RankedResult{
		{ID: "1", Content: "bounded pattern", Score: 0.5, Project: "memory"},
	}
	decayed := h.applyTurnBasedDecay(results, "memory")
	if len(decayed) != 1 {
		t.Fatalf("result count = %d, want 1", len(decayed))
	}
	if decayed[0].Score >= 0.5 {
		t.Fatalf("a learning inside its TTL must still decay (100 turns since creation), score = %.4f", decayed[0].Score)
	}
}

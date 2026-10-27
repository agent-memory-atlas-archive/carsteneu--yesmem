package daemon

import (
	"context"
	"fmt"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestSkipRecentSurvivesForeignInsertsLos10c (YM-305 [MEM-F37]): skip_recent was a
// global window over the newest IDs. Three learnings written by any other session
// pushed the caller's own just-extracted learning out of it, and the associative
// context then injected that learning back into the turn that produced it.
func TestSkipRecentSurvivesForeignInsertsLos10c(t *testing.T) {
	h, s, indexer := mustVectorHandler(t)
	const victimSession = "sess-los10c-victim"

	victimID := insertAgedLearning(t, s, &models.Learning{
		SessionID: victimSession,
		Content:   "los10c skip anchor victim",
		Category:  "gotcha",
		Source:    "llm_extracted",
	})
	if err := indexer.Index(context.Background(), fmt.Sprintf("%d", victimID), "los10c skip anchor victim", nil); err != nil {
		t.Fatalf("index victim: %v", err)
	}

	// Three newer rows from another session, matching the same query.
	for _, word := range []string{"alfa", "bravo", "charlie"} {
		id := insertAgedLearning(t, s, &models.Learning{
			SessionID: "sess-los10c-other",
			Content:   "los10c skip anchor " + word,
			Category:  "gotcha",
			Source:    "llm_extracted",
		})
		if err := indexer.Index(context.Background(), fmt.Sprintf("%d", id), "los10c skip anchor "+word, nil); err != nil {
			t.Fatalf("index %s: %v", word, err)
		}
	}

	control := hybridSearchParams(t, h, map[string]any{"query": "los10c skip anchor"})
	if !control[fmt.Sprintf("%d", victimID)] {
		t.Fatalf("control failed: the victim learning must be reachable without skip_recent: %v", control)
	}

	ids := hybridSearchParams(t, h, map[string]any{
		"query":       "los10c skip anchor",
		"skip_recent": float64(1),
		"session_id":  victimSession,
	})
	if ids[fmt.Sprintf("%d", victimID)] {
		t.Errorf("learning %d is the newest of its own session and must stay skipped; three foreign inserts moved the global window", victimID)
	}
}

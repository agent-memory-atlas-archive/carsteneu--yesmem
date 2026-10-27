package daemon

import (
	"fmt"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// los10cMaxHybridSearchLimit mirrors the cap the handler must enforce. Kept as a
// literal so the RED run compiles against the unpatched handler.
const los10cMaxHybridSearchLimit = 200

// TestHybridSearchClampsCallerLimitLos10c (YM-305 [MEM-F37]): hybrid_search used the
// caller's limit for its slices, for the lane size and for the AQ lane's limit*3.
// A negative limit panicked the handler; an absurd one let the whole lane through,
// whose size is derived from the corpus and not from the caller. The rows stay
// unindexed on purpose: the bm25_only branch is where the slice and the lane meet.
func TestHybridSearchClampsCallerLimitLos10c(t *testing.T) {
	h, s, _ := mustVectorHandler(t)
	const rows = 250
	for i := 0; i < rows; i++ {
		insertAgedLearning(t, s, &models.Learning{
			Content:  fmt.Sprintf("los10c limit anchor %d", i),
			Category: "gotcha",
			Source:   "llm_extracted",
		})
	}

	for _, tc := range []struct {
		name  string
		limit float64
	}{
		{"negative", -5},
		{"huge", float64(int64(1) << 61)},
		{"zero", 0},
		{"over-cap", 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.Handle(Request{
				Method: "hybrid_search",
				Params: map[string]any{
					"query": "los10c limit anchor",
					"limit": tc.limit,
				},
			})
			if resp.Error != "" {
				t.Fatalf("hybrid_search with limit=%v: %s", tc.limit, resp.Error)
			}
			got := hybridSearchParams(t, h, map[string]any{"query": "los10c limit anchor", "limit": tc.limit})
			if len(got) > los10cMaxHybridSearchLimit {
				t.Errorf("limit=%v returned %d results, want at most %d", tc.limit, len(got), los10cMaxHybridSearchLimit)
			}
			if tc.limit <= 0 && len(got) != 0 {
				t.Errorf("limit=%v returned %d results, want none", tc.limit, len(got))
			}
		})
	}
}

// TestHybridSearchZeroLimitReturnsNothingLos10c: with a vector lane present the
// merge runs through RRFMerge, which truncates only for a positive limit. A zero
// limit therefore returned the whole lane instead of nothing, unlike get_learnings.
func TestHybridSearchZeroLimitReturnsNothingLos10c(t *testing.T) {
	h, s, indexer := mustVectorHandler(t)
	// The lane size is max(limit, corpus/10), so the corpus has to be large enough
	// for a zero limit to leave a non-zero lane behind.
	for i := 0; i < 40; i++ {
		content := fmt.Sprintf("los10c zero limit %d", i)
		id := insertAgedLearning(t, s, &models.Learning{
			Content:  content,
			Category: "gotcha",
			Source:   "llm_extracted",
		})
		if i < 3 {
			if err := indexer.Index(t.Context(), fmt.Sprintf("%d", id), content, nil); err != nil {
				t.Fatalf("index %d: %v", id, err)
			}
		}
	}

	if control := hybridSearchParams(t, h, map[string]any{"query": "los10c zero limit", "limit": float64(5)}); len(control) == 0 {
		t.Fatal("control failed: the fixtures must be reachable with a positive limit")
	}
	got := hybridSearchParams(t, h, map[string]any{"query": "los10c zero limit", "limit": float64(0)})
	if len(got) != 0 {
		t.Errorf("limit=0 returned %d results, want none", len(got))
	}
}

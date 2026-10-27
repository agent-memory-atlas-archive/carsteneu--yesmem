package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los10bOffset is deliberately ahead of UTC: a lexicographic comparison against
// a UTC bound then flips the verdict, while the parsed instants do not.
var los10bOffset = time.FixedZone("LOS10B", 2*3600)

// hybridSearchParams runs hybrid_search with extra params and returns the set of
// result IDs.
func hybridSearchParams(t *testing.T, h *Handler, params map[string]any) map[string]bool {
	t.Helper()
	if _, ok := params["query"]; !ok {
		params["query"] = "zzz qqq"
	}
	if _, ok := params["limit"]; !ok {
		params["limit"] = float64(20)
	}
	resp := h.Handle(Request{Method: "hybrid_search", Params: params})
	if resp.Error != "" {
		t.Fatalf("hybrid_search error: %s", resp.Error)
	}
	var out struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("parse hybrid_search result: %v", err)
	}
	ids := make(map[string]bool, len(out.Results))
	for _, r := range out.Results {
		ids[r.ID] = true
	}
	return ids
}

// TestHybridSearchComparesParsedTimestampsLos10b (YM-246 [MEM-F29]): since and
// before were compared as strings. created_at is RFC3339 in the writer's zone,
// so an offset of two hours flipped inclusion for a bound only minutes away.
func TestHybridSearchComparesParsedTimestampsLos10b(t *testing.T) {
	// One hour old, written with +02:00: its wall clock reads one hour AHEAD of
	// UTC, its instant is one hour in the past.
	createdAt := time.Now().Add(-time.Hour).In(los10bOffset)
	bound := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)

	t.Run("since excludes an older learning", func(t *testing.T) {
		h, s, indexer := mustVectorHandler(t)
		id := insertAgedLearning(t, s, &models.Learning{
			Content:   "los10b since sentinel",
			Category:  "gotcha",
			Source:    "llm_extracted",
			CreatedAt: createdAt,
		})
		if err := indexer.Index(context.Background(), fmt.Sprintf("%d", id), "los10b since sentinel", nil); err != nil {
			t.Fatalf("index: %v", err)
		}

		ids := hybridSearchParams(t, h, map[string]any{"since": bound})
		if ids[fmt.Sprintf("%d", id)] {
			t.Errorf("learning created %s is older than since=%s but was returned", createdAt.Format(time.RFC3339), bound)
		}
	})

	t.Run("before keeps an older learning", func(t *testing.T) {
		h, s, indexer := mustVectorHandler(t)
		id := insertAgedLearning(t, s, &models.Learning{
			Content:   "los10b before sentinel",
			Category:  "gotcha",
			Source:    "llm_extracted",
			CreatedAt: createdAt,
		})
		if err := indexer.Index(context.Background(), fmt.Sprintf("%d", id), "los10b before sentinel", nil); err != nil {
			t.Fatalf("index: %v", err)
		}

		ids := hybridSearchParams(t, h, map[string]any{"before": bound})
		if !ids[fmt.Sprintf("%d", id)] {
			t.Errorf("learning created %s is older than before=%s but was dropped", createdAt.Format(time.RFC3339), bound)
		}
	})
}

// TestSearchBoundsRejectUnparseableValuesLos10b (YM-246 [MEM-F29]): a bound that
// cannot be parsed used to be compared as a string, which filtered by accident
// and by luck. It is now rejected instead of silently returning an unfiltered
// result the caller believes to be filtered.
func TestSearchBoundsRejectUnparseableValuesLos10b(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.Handle(Request{
		Method: "hybrid_search",
		Params: map[string]any{"query": "los10b bound probe", "since": "gestern"},
	})
	if resp.Error == "" {
		t.Error("hybrid_search accepted since=gestern")
	}

	resp = h.Handle(Request{
		Method: "get_learnings",
		Params: map[string]any{"before": "last week"},
	})
	if resp.Error == "" {
		t.Error("get_learnings accepted before=last week")
	}

	// Control: the documented forms still work.
	for _, bound := range []string{"2026-07-01", "2026-07-01T00:00:00Z", "2026-07-01T00:00:00+02:00"} {
		resp := h.Handle(Request{
			Method: "hybrid_search",
			Params: map[string]any{"query": "los10b bound probe", "since": bound},
		})
		if resp.Error != "" {
			t.Errorf("hybrid_search rejected the documented bound %q: %s", bound, resp.Error)
		}
	}
}

// TestHybridSearchSkipRecentDropsFreshHitsLos10b (YM-246 [MEM-F29]): the proxy's
// associative context sends skip_recent so a learning cannot be injected into
// the turn that just extracted it. The daemon ignored the parameter.
func TestHybridSearchSkipRecentDropsFreshHitsLos10b(t *testing.T) {
	h, s, indexer := mustVectorHandler(t)

	olderID := insertAgedLearning(t, s, &models.Learning{
		Content:  "los10b skip recent anchor alpha",
		Category: "gotcha",
		Source:   "llm_extracted",
	})
	newerID := insertAgedLearning(t, s, &models.Learning{
		Content:  "los10b skip recent anchor beta",
		Category: "gotcha",
		Source:   "llm_extracted",
	})
	for _, id := range []int64{olderID, newerID} {
		if err := indexer.Index(context.Background(), fmt.Sprintf("%d", id), "los10b skip recent anchor", nil); err != nil {
			t.Fatalf("index %d: %v", id, err)
		}
	}

	control := hybridSearchParams(t, h, map[string]any{})
	if !control[fmt.Sprintf("%d", olderID)] || !control[fmt.Sprintf("%d", newerID)] {
		t.Fatalf("control failed: both learnings must be reachable without skip_recent: %v", control)
	}

	ids := hybridSearchParams(t, h, map[string]any{"skip_recent": float64(1)})
	if ids[fmt.Sprintf("%d", newerID)] {
		t.Errorf("learning %d is one of the 1 most recent and must be skipped", newerID)
	}
	if !ids[fmt.Sprintf("%d", olderID)] {
		t.Errorf("older learning %d must survive skip_recent=1", olderID)
	}
}

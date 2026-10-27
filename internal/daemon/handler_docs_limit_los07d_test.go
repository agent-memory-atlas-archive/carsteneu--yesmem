package daemon

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los07dContextualDefaultLimit is the limit the handler must fall back to when
// the caller supplies none. Kept as a literal so the RED run compiles against
// the unpatched handler (which has no such constant).
const los07dContextualDefaultLimit = 3

// los07dContextualMaxLimit is the cap the handler must enforce on a caller
// supplied limit, and los07dContextualChunks is more than that cap so the bound
// is observable.
const (
	los07dContextualMaxLimit = 100
	los07dContextualChunks   = 120
)

// los07dSeedContextualChunks creates one reference source with more chunks than
// the cap allows. All chunks carry the same token count, so the storage ORDER BY
// has no tie-breaker and the row order is arbitrary; the tests below assert
// counts only.
func los07dSeedContextualChunks(t *testing.T, s *storage.Store) {
	t.Helper()
	id, err := s.UpsertDocSource(&storage.DocSource{
		Name:              "los07d-docs",
		Project:           "los07d",
		DocType:           "reference",
		TriggerExtensions: ".go",
	})
	if err != nil {
		t.Fatalf("upsert doc source: %v", err)
	}
	for i := 0; i < los07dContextualChunks; i++ {
		if _, err := s.InsertDocChunk(&storage.DocChunk{
			SourceID:     id,
			SourceFile:   "los07d.md",
			HeadingPath:  "Limits",
			Content:      fmt.Sprintf("contextual chunk %d", i),
			ContentHash:  fmt.Sprintf("los07d-ctx-%d", i),
			TokensApprox: 50,
		}); err != nil {
			t.Fatalf("insert doc chunk %d: %v", i, err)
		}
	}
}

func los07dContextualTotal(t *testing.T, h *Handler, limit any) int {
	t.Helper()
	params := map[string]any{
		"extensions": []any{".go"},
		"project":    "los07d",
	}
	if limit != nil {
		params["limit"] = limit
	}
	resp := h.Handle(Request{Method: "contextual_docs", Params: params})
	if resp.Error != "" {
		t.Fatalf("contextual_docs error: %s", resp.Error)
	}
	var result struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return result.Total
}

// YM-324 [DEV-DT50]: a negative limit was passed straight through to
// GetDocChunksBySourceIDs, and SQLite reads LIMIT -1 as "no limit". The proxy's
// doc-extension fallback (internal/proxy/associate.go, limit 1) therefore had no
// bound on how much documentation a matching extension could pull in; an absurd
// positive limit had none either.
func TestHandleContextualDocs_NegativeLimitIsBoundedLos07d(t *testing.T) {
	h, s := mustHandler(t)
	los07dSeedContextualChunks(t, s)

	if got := los07dContextualTotal(t, h, nil); got != los07dContextualDefaultLimit {
		t.Fatalf("no limit supplied: total = %d, want the default %d", got, los07dContextualDefaultLimit)
	}

	for _, limit := range []float64{-1, -1000} {
		if got := los07dContextualTotal(t, h, limit); got != los07dContextualDefaultLimit {
			t.Errorf("limit %v: total = %d, want the default %d (a negative limit must not mean unlimited)",
				limit, got, los07dContextualDefaultLimit)
		}
	}

	// A caller supplied limit inside the allowed range still wins, and 0 keeps
	// meaning "no results".
	if got := los07dContextualTotal(t, h, float64(2)); got != 2 {
		t.Errorf("limit 2: total = %d, want 2", got)
	}
	if got := los07dContextualTotal(t, h, float64(0)); got != 0 {
		t.Errorf("limit 0: total = %d, want 0", got)
	}

	// Above the cap the response is bounded; below it the caller decides.
	if got := los07dContextualTotal(t, h, float64(1e9)); got != los07dContextualMaxLimit {
		t.Errorf("limit 1e9: total = %d, want the cap %d", got, los07dContextualMaxLimit)
	}
	if got := los07dContextualTotal(t, h, float64(99)); got != 99 {
		t.Errorf("limit 99: total = %d, want 99", got)
	}
}

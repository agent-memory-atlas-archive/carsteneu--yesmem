package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los07cMaxDocsSearchLimit is the cap the handler must enforce on a caller
// supplied limit. Kept as a literal so the RED run compiles against the
// unpatched handler (which has no such constant).
const los07cMaxDocsSearchLimit = 1000

// los07cMaxQueryRunes is the query length the handler must accept. Kept as a
// literal for the same reason.
const los07cMaxQueryRunes = 2000

// los07cCallHandlerNoPanic runs one daemon request on its own goroutine so a
// panic inside the handler is reported as a failed test instead of killing the
// test binary (a panic here is the finding, not a fixture problem).
func los07cCallHandlerNoPanic(t *testing.T, h *Handler, req Request) (Response, any) {
	t.Helper()
	type outcome struct {
		resp Response
		rec  any
	}
	done := make(chan outcome, 1)
	go func() {
		o := outcome{}
		defer func() {
			o.rec = recover()
			done <- o
		}()
		o.resp = h.Handle(req)
	}()
	select {
	case o := <-done:
		return o.resp, o.rec
	case <-time.After(10 * time.Second):
		t.Fatalf("docs handler did not return within 10s")
		return Response{}, nil
	}
}

// los07cSeedDocs creates a source with a few indexable chunks.
func los07cSeedDocs(t *testing.T, s *storage.Store) {
	t.Helper()
	id, err := s.UpsertDocSource(&storage.DocSource{Name: "los07c-docs", Project: "", DocType: "reference"})
	if err != nil {
		t.Fatalf("upsert doc source: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := s.InsertDocChunk(&storage.DocChunk{
			SourceID:     id,
			SourceFile:   "doc.md",
			HeadingPath:  "Limits",
			Content:      "golang interface embedding struct limit",
			ContentHash:  "los07c-lim-" + string(rune('a'+i)),
			TokensApprox: 50,
		}); err != nil {
			t.Fatalf("insert doc chunk: %v", err)
		}
	}
}

// YM-294 [DEV-DT46]: a negative limit reached merged[:limit] and panicked the
// handler goroutine. intOr performs no clamp and the dispatch switch has no
// recover, so one request would take the whole daemon down. A negative limit is
// a caller error and must come back as one, not as an empty result set.
func TestHandleDocsSearch_NegativeLimitDoesNotPanicLos07c(t *testing.T) {
	h, s := mustHandler(t)
	los07cSeedDocs(t, s)

	resp, rec := los07cCallHandlerNoPanic(t, h, Request{
		Method: "docs_search",
		Params: map[string]any{
			"query": "golang interface embedding",
			"limit": float64(-5),
		},
	})
	if rec != nil {
		t.Fatalf("docs_search with limit=-5 panicked: %v", rec)
	}
	if resp.Error == "" {
		t.Fatalf("docs_search accepted limit=-5 and answered with %s", string(resp.Result))
	}
}

// YM-294 [DEV-DT46]: the limit is a float64 from JSON. A value above the int64
// range converts to a negative int, so the upper bound has to be clamped as
// well; without it the same slice panic returns through a different door.
func TestHandleDocsSearch_AbsurdLimitDoesNotPanicLos07c(t *testing.T) {
	h, s := mustHandler(t)
	los07cSeedDocs(t, s)

	// 1e19 is above math.MaxInt64: int(float64) overflows to a negative value.
	resp, rec := los07cCallHandlerNoPanic(t, h, Request{
		Method: "docs_search",
		Params: map[string]any{
			"query": "golang interface embedding",
			"limit": float64(1e19),
		},
	})
	if rec != nil {
		t.Fatalf("docs_search with an absurd limit panicked: %v", rec)
	}
	if resp.Error != "" {
		t.Fatalf("docs_search with an absurd limit: %s", resp.Error)
	}

	var result struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Total > los07cMaxDocsSearchLimit {
		t.Errorf("absurd limit returned %d results, want at most %d", result.Total, los07cMaxDocsSearchLimit)
	}
}

// YM-294 [DEV-DT46]: the query length was unbounded. Every call pays a BM25
// pass plus one embedding request, so a pasted document is an unbounded cost
// per call. The handler must refuse it with a clear message.
func TestHandleDocsSearch_RejectsOverlongQueryLos07c(t *testing.T) {
	h, s := mustHandler(t)
	los07cSeedDocs(t, s)

	overlong := strings.Repeat("interface embedding ", los07cMaxQueryRunes)
	if got := len([]rune(overlong)); got <= los07cMaxQueryRunes {
		t.Fatalf("fixture precondition: query has %d runes, want more than %d", got, los07cMaxQueryRunes)
	}

	resp, rec := los07cCallHandlerNoPanic(t, h, Request{
		Method: "docs_search",
		Params: map[string]any{
			"query": overlong,
		},
	})
	if rec != nil {
		t.Fatalf("docs_search with an overlong query panicked: %v", rec)
	}
	if resp.Error == "" {
		t.Fatalf("docs_search accepted a query of %d runes (cap %d)", len([]rune(overlong)), los07cMaxQueryRunes)
	}
}

// The cap has to be pinned by value, not only by "no panic": without this a
// change to the cap or a dropped clamp branch stays invisible while every other
// test still passes.
func TestHandleDocsSearch_ClampsTheLimitToTheCapLos07c(t *testing.T) {
	h, s := mustHandler(t)

	id, err := s.UpsertDocSource(&storage.DocSource{Name: "cap-docs", Project: "", DocType: "reference"})
	if err != nil {
		t.Fatalf("upsert doc source: %v", err)
	}
	// One more chunk than the cap, so a missing clamp would show up as an extra
	// result.
	for i := 0; i <= los07cMaxDocsSearchLimit; i++ {
		if _, err := s.InsertDocChunk(&storage.DocChunk{
			SourceID:     id,
			SourceFile:   "doc.md",
			HeadingPath:  "Cap",
			Content:      "golang interface embedding struct cap",
			ContentHash:  fmt.Sprintf("los07c-cap-%d", i),
			TokensApprox: 50,
		}); err != nil {
			t.Fatalf("insert doc chunk %d: %v", i, err)
		}
	}

	resp, rec := los07cCallHandlerNoPanic(t, h, Request{
		Method: "docs_search",
		Params: map[string]any{
			"query": "golang interface embedding",
			"limit": float64(los07cMaxDocsSearchLimit * 10),
		},
	})
	if rec != nil {
		t.Fatalf("docs_search panicked: %v", rec)
	}
	if resp.Error != "" {
		t.Fatalf("docs_search: %s", resp.Error)
	}

	var result struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Total != los07cMaxDocsSearchLimit {
		t.Errorf("total = %d, want exactly the cap %d", result.Total, los07cMaxDocsSearchLimit)
	}
}

// The cap must not change what a normal query does.
func TestHandleDocsSearch_AcceptsQueryAtTheCapLos07c(t *testing.T) {
	h, s := mustHandler(t)
	los07cSeedDocs(t, s)

	// Exactly at the cap: padding keeps the searchable terms intact.
	atCap := "golang interface embedding" + strings.Repeat(" ", los07cMaxQueryRunes-len("golang interface embedding"))

	resp, rec := los07cCallHandlerNoPanic(t, h, Request{
		Method: "docs_search",
		Params: map[string]any{
			"query": atCap,
		},
	})
	if rec != nil {
		t.Fatalf("docs_search at the cap panicked: %v", rec)
	}
	if resp.Error != "" {
		t.Fatalf("docs_search rejected a query at the cap: %s", resp.Error)
	}
}

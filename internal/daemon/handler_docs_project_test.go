package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// fixedEmbedProvider answers every embedding request with the same vector.
type fixedEmbedProvider struct{ vec []float32 }

func (f *fixedEmbedProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}
func (f *fixedEmbedProvider) Dimensions() int { return len(f.vec) }
func (f *fixedEmbedProvider) Enabled() bool   { return true }
func (f *fixedEmbedProvider) Close() error    { return nil }

type docsSearchResult struct {
	Results []struct {
		Source string `json:"source"`
		ID     int64  `json:"id"`
	} `json:"results"`
	Total  int    `json:"total"`
	Method string `json:"method"`
}

func runDocsSearch(t *testing.T, h *Handler, params map[string]any) docsSearchResult {
	t.Helper()
	resp := h.Handle(Request{Method: "docs_search", Params: params})
	if resp.Error != "" {
		t.Fatalf("docs_search error: %s", resp.Error)
	}
	var res docsSearchResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return res
}

// YM-28: the result list was cut to `limit` BEFORE the project filter ran, so a
// query whose unfiltered top hits belong to a foreign project came back empty
// even though matching chunks of the requested project existed.
func TestHandleDocsSearch_ProjectFilterAppliedBeforeLimit(t *testing.T) {
	h, s := mustHandler(t)

	mine, _ := s.UpsertDocSource(&storage.DocSource{Name: "mine-docs", Project: "proj", Version: "1", DocType: "reference"})
	foreign, _ := s.UpsertDocSource(&storage.DocSource{Name: "foreign-docs", Project: "elsewhere", Version: "1", DocType: "reference"})

	// Foreign chunks are short and term-dense, so they win BM25 outright.
	s.InsertDocChunk(&storage.DocChunk{SourceID: foreign, SourceFile: "f1.md", HeadingPath: "Foreign", Content: "gadget gadget gadget", ContentHash: "pf1", TokensApprox: 5})
	s.InsertDocChunk(&storage.DocChunk{SourceID: foreign, SourceFile: "f2.md", HeadingPath: "Foreign", Content: "gadget gadget", ContentHash: "pf2", TokensApprox: 5})
	s.InsertDocChunk(&storage.DocChunk{SourceID: foreign, SourceFile: "f3.md", HeadingPath: "Foreign", Content: "gadget", ContentHash: "pf3", TokensApprox: 5})
	// The requested project's own chunk is long, so it ranks behind them.
	s.InsertDocChunk(&storage.DocChunk{SourceID: mine, SourceFile: "m1.md", HeadingPath: "Mine", Content: "gadget and a good deal of further prose that dilutes the term frequency", ContentHash: "pm1", TokensApprox: 60})

	res := runDocsSearch(t, h, map[string]any{
		"query":   "gadget",
		"project": "proj",
		"limit":   float64(1),
	})
	if res.Total != 1 {
		t.Fatalf("total = %d, want 1 — the project's own hit was cut by the limit before the filter", res.Total)
	}
	if res.Results[0].Source != "mine-docs" {
		t.Errorf("source = %q, want mine-docs", res.Results[0].Source)
	}
}

// YM-28: the vector fallback ignored the project scope completely, so a query
// with no lexical match could return another project's documentation.
func TestHandleDocsSearch_SemanticRespectsProject(t *testing.T) {
	h, s := mustHandler(t)

	provider := &fixedEmbedProvider{vec: []float32{1, 0, 0, 0}}
	h.SetEmbedding(nil, nil, provider)

	mine, _ := s.UpsertDocSource(&storage.DocSource{Name: "mine-docs", Project: "proj", Version: "1", DocType: "reference"})
	foreign, _ := s.UpsertDocSource(&storage.DocSource{Name: "foreign-docs", Project: "elsewhere", Version: "1", DocType: "reference"})

	mineID, err := s.InsertDocChunk(&storage.DocChunk{SourceID: mine, SourceFile: "m1.md", HeadingPath: "Mine", Content: "alpha note", ContentHash: "sm1", TokensApprox: 5})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	foreignID, err := s.InsertDocChunk(&storage.DocChunk{SourceID: foreign, SourceFile: "f1.md", HeadingPath: "Foreign", Content: "beta note", ContentHash: "sf1", TokensApprox: 5})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	// The foreign chunk matches the query vector exactly, the project's own one
	// only closely — so it would win the top-1 slot without the scope filter.
	if err := s.SetDocChunkEmbedding(foreignID, []float32{1, 0, 0, 0}, "sf1"); err != nil {
		t.Fatalf("embed foreign: %v", err)
	}
	if err := s.SetDocChunkEmbedding(mineID, []float32{0.99, 0.14, 0, 0}, "sm1"); err != nil {
		t.Fatalf("embed mine: %v", err)
	}

	// No lexical match for either chunk, so BM25 is empty and the vector path runs.
	res := runDocsSearch(t, h, map[string]any{
		"query":   "zzzqqq",
		"project": "proj",
		"limit":   float64(1),
	})
	if res.Method != "semantic" {
		t.Fatalf("method = %q, want semantic", res.Method)
	}
	if res.Total != 1 {
		t.Fatalf("total = %d, want 1 — the vector path ignored the project scope", res.Total)
	}
	if res.Results[0].Source != "mine-docs" {
		t.Errorf("source = %q, want mine-docs (foreign project leaked into the vector path)", res.Results[0].Source)
	}
}

// The extension filter and the project scope are intersected: a global source
// matching the extension must survive, a foreign project's source must not.
func TestHandleDocsSearch_ExtensionsWithProjectScope(t *testing.T) {
	h, s := mustHandler(t)

	mine, _ := s.UpsertDocSource(&storage.DocSource{Name: "mine-go", Project: "proj", TriggerExtensions: ".go", DocType: "reference"})
	global, _ := s.UpsertDocSource(&storage.DocSource{Name: "global-go", Project: "", TriggerExtensions: ".go", DocType: "reference"})
	foreign, _ := s.UpsertDocSource(&storage.DocSource{Name: "foreign-go", Project: "elsewhere", TriggerExtensions: ".go", DocType: "reference"})

	s.InsertDocChunk(&storage.DocChunk{SourceID: mine, SourceFile: "m.md", HeadingPath: "H", Content: "goroutine scheduler notes", ContentHash: "xp1", TokensApprox: 20})
	s.InsertDocChunk(&storage.DocChunk{SourceID: global, SourceFile: "g.md", HeadingPath: "H", Content: "goroutine scheduler notes", ContentHash: "xp2", TokensApprox: 20})
	s.InsertDocChunk(&storage.DocChunk{SourceID: foreign, SourceFile: "f.md", HeadingPath: "H", Content: "goroutine scheduler notes", ContentHash: "xp3", TokensApprox: 20})

	res := runDocsSearch(t, h, map[string]any{
		"query":      "goroutine scheduler",
		"extensions": []any{".go"},
		"project":    "proj",
		"limit":      float64(10),
	})
	if res.Total == 0 {
		t.Fatalf("expected hits from the project and global sources")
	}
	saw := map[string]bool{}
	for _, r := range res.Results {
		saw[r.Source] = true
		if r.Source == "foreign-go" {
			t.Errorf("foreign project source leaked into a scoped search")
		}
	}
	if !saw["mine-go"] {
		t.Errorf("project source missing from results: %v", saw)
	}
	if !saw["global-go"] {
		t.Errorf("global source dropped by the project scope: %v", saw)
	}
}

// The scope must keep the BEST hit, not just any hit: BM25 scores from FTS5 are
// negative (more negative = better), so a descending sort on raw scores inverts
// relevance. The boost is keyed on the chunk's source id, not the chunk id.
func TestHandleDocsSearch_ProjectScopeKeepsBestHits(t *testing.T) {
	h, s := mustHandler(t)

	// Insertion order is deliberate: the project source gets a higher id than the
	// project's chunk, so a chunk-id-keyed boost hits the foreign chunk instead.
	global, _ := s.UpsertDocSource(&storage.DocSource{Name: "global-docs", Project: "", Version: "1", DocType: "reference"})
	mine, _ := s.UpsertDocSource(&storage.DocSource{Name: "mine-docs", Project: "proj", Version: "1", DocType: "reference"})

	// Best match belongs to the project, worst to the global source.
	mineChunk, err := s.InsertDocChunk(&storage.DocChunk{SourceID: mine, SourceFile: "m.md", HeadingPath: "Mine", Content: "gadget gadget gadget", ContentHash: "rk1", TokensApprox: 5})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	globalChunk, err := s.InsertDocChunk(&storage.DocChunk{SourceID: global, SourceFile: "g.md", HeadingPath: "Global", Content: "gadget surrounded by a good deal of unrelated prose that dilutes the term", ContentHash: "rk2", TokensApprox: 60})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	if mineChunk == mine {
		t.Fatalf("fixture precondition: project chunk id %d must differ from its source id %d", mineChunk, mine)
	}
	if globalChunk != mine {
		t.Fatalf("fixture precondition: global chunk id %d must equal the project source id %d to expose the collision", globalChunk, mine)
	}

	res := runDocsSearch(t, h, map[string]any{
		"query":   "gadget",
		"project": "proj",
		"limit":   float64(1),
	})
	if res.Total != 1 {
		t.Fatalf("total = %d, want 1", res.Total)
	}
	if res.Results[0].Source != "mine-docs" {
		t.Fatalf("top hit = %q, want mine-docs — the best BM25 hit was sorted away", res.Results[0].Source)
	}
}

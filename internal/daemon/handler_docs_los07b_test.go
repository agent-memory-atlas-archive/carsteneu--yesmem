package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/ingest"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-231: handleRemoveDocs derived the clone directory from the caller-supplied
// source name and removed it recursively. A name that is not a single path
// element (here a source row named "../victim") escaped the docs directory and
// deleted an unrelated tree under the data dir over MCP.
func TestHandleRemoveDocs_RejectsNamesThatEscapeTheDocsDir(t *testing.T) {
	h, s := mustHandler(t)

	victim := filepath.Join(h.dataDir, "victim")
	if err := os.MkdirAll(victim, 0755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	keep := filepath.Join(victim, "keep.txt")
	if err := os.WriteFile(keep, []byte("must survive"), 0644); err != nil {
		t.Fatalf("write keep: %v", err)
	}

	if _, err := s.UpsertDocSource(&storage.DocSource{Name: "../victim", Project: "p", Path: filepath.Join(h.dataDir, "src")}); err != nil {
		t.Fatalf("upsert source: %v", err)
	}

	resp := h.Handle(Request{Method: "remove_docs", Params: map[string]any{
		"name":    "../victim",
		"project": "p",
	}})
	if resp.Error == "" {
		t.Fatalf("remove_docs accepted a traversal name; response = %s", string(resp.Result))
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("file outside the docs directory was deleted: %v", err)
	}
	if _, err := s.GetDocSource("../victim", "p"); err != nil {
		t.Errorf("source row was removed although the name was rejected: %v", err)
	}
}

// YM-231: a relative path sent over MCP was resolved against the daemon's own
// working directory, which is not the caller's directory. The source then held
// whatever tree sat at that name next to the daemon.
func TestHandleIngestDocs_RejectsRelativePath(t *testing.T) {
	h, s := mustHandler(t)

	resp := h.Handle(Request{Method: "ingest_docs", Params: map[string]any{
		"name": "rel-docs",
		"path": "docs",
	}})
	if resp.Error == "" {
		t.Fatalf("ingest_docs accepted a relative path; response = %s", string(resp.Result))
	}

	sources, err := s.ListDocSources("")
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("%d source(s) registered from a relative path", len(sources))
	}
}

// YM-231: registration accepted any name while deletion required a single path
// element, so an agent could create a source it could never remove again.
func TestHandleIngestDocs_RejectsInvalidName(t *testing.T) {
	h, s := mustHandler(t)

	resp := h.Handle(Request{Method: "ingest_docs", Params: map[string]any{
		"name": "../../escape",
		"path": t.TempDir(),
	}})
	if resp.Error == "" {
		t.Fatalf("ingest_docs accepted an invalid name; response = %s", string(resp.Result))
	}

	sources, err := s.ListDocSources("")
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("%d source(s) registered with a name that remove-docs rejects", len(sources))
	}
}

// YM-231: a relative legacy path is skipped by both sync loops and ignored by
// --prune (it cannot be verified), so list_docs is the only place the state can
// become visible.
func TestHandleListDocs_FlagsRelativePath(t *testing.T) {
	h, s := mustHandler(t)

	if _, err := s.UpsertDocSource(&storage.DocSource{Name: "relative-docs", Project: "p", Path: "docs"}); err != nil {
		t.Fatalf("upsert source: %v", err)
	}

	resp := h.Handle(Request{Method: "list_docs", Params: map[string]any{"project": "p"}})
	if resp.Error != "" {
		t.Fatalf("list_docs error: %s", resp.Error)
	}
	var body struct {
		Sources []struct {
			Name         string `json:"name"`
			PathRelative bool   `json:"path_relative"`
			PathMissing  bool   `json:"path_missing"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(body.Sources))
	}
	if !body.Sources[0].PathRelative {
		t.Errorf("relative path not flagged: %+v", body.Sources[0])
	}
	if body.Sources[0].PathMissing {
		t.Errorf("relative path reported as missing, which --prune would act on: %+v", body.Sources[0])
	}
}

// YM-231: the clone directory of a URL import lives under the data dir, not
// under $HOME; removal has to find it there.
func TestHandleRemoveDocs_RemovesCloneDirUnderDataDir(t *testing.T) {
	h, s := mustHandler(t)

	cloneDir, err := ingest.CloneDir(h.dataDir, "url-docs")
	if err != nil {
		t.Fatalf("clone dir: %v", err)
	}
	if err := os.MkdirAll(cloneDir, 0755); err != nil {
		t.Fatalf("mkdir clone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "guide.md"), []byte("# Guide"), 0644); err != nil {
		t.Fatalf("write guide: %v", err)
	}

	if _, err := s.UpsertDocSource(&storage.DocSource{Name: "url-docs", Project: "p", Path: cloneDir, URL: "https://example.invalid/docs"}); err != nil {
		t.Fatalf("upsert source: %v", err)
	}

	resp := h.Handle(Request{Method: "remove_docs", Params: map[string]any{
		"name":    "url-docs",
		"project": "p",
	}})
	if resp.Error != "" {
		t.Fatalf("remove_docs error: %s", resp.Error)
	}
	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Errorf("clone directory still exists after remove_docs: %v", err)
	}
}

// YM-231: list_docs showed a source with a vanished path like any other, so the
// only way to notice was to search it and get nothing.
func TestHandleListDocs_FlagsMissingPath(t *testing.T) {
	h, s := mustHandler(t)

	aliveDir := t.TempDir()
	goneDir := filepath.Join(t.TempDir(), "gone")
	for _, src := range []storage.DocSource{
		{Name: "live-docs", Project: "p", Path: aliveDir},
		{Name: "dead-docs", Project: "p", Path: goneDir},
	} {
		if _, err := s.UpsertDocSource(&src); err != nil {
			t.Fatalf("upsert %s: %v", src.Name, err)
		}
	}

	resp := h.Handle(Request{Method: "list_docs", Params: map[string]any{"project": "p"}})
	if resp.Error != "" {
		t.Fatalf("list_docs error: %s", resp.Error)
	}
	var body struct {
		Sources []struct {
			Name        string `json:"name"`
			Path        string `json:"path"`
			PathMissing bool   `json:"path_missing"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(body.Sources))
	}
	for _, item := range body.Sources {
		switch item.Name {
		case "dead-docs":
			if !item.PathMissing {
				t.Errorf("dead path not flagged: %+v", item)
			}
			if item.Path != goneDir {
				t.Errorf("path = %q, want %q", item.Path, goneDir)
			}
		case "live-docs":
			if item.PathMissing {
				t.Errorf("live path flagged as missing: %+v", item)
			}
		}
	}
}

// YM-231: the RRF merge became the normal path, and its scores collide
// routinely (two results in swapped ranks sum to the same value). The result
// order must not depend on Go's randomized map iteration.
func TestMergeDocResults_StableOrderOnEqualScores(t *testing.T) {
	mk := func(ids ...int64) []storage.DocChunkResult {
		out := make([]storage.DocChunkResult, 0, len(ids))
		for _, id := range ids {
			out = append(out, storage.DocChunkResult{DocChunk: storage.DocChunk{ID: id}})
		}
		return out
	}
	bm25 := mk(9, 3, 5, 7)
	vector := mk(3, 9, 7, 5)

	var first []int64
	for i := 0; i < 100; i++ {
		got := mergeDocResults(bm25, vector, 10)
		ids := make([]int64, len(got))
		for j := range got {
			ids[j] = got[j].ID
		}
		if i == 0 {
			first = ids
			continue
		}
		for j := range ids {
			if ids[j] != first[j] {
				t.Fatalf("order changed between calls: %v then %v", first, ids)
			}
		}
	}
}

// YM-231: the vector path only ran when BM25 returned nothing, so the RRF merge
// and method "hybrid" were unreachable. A query with lexical hits never saw a
// semantically matching chunk from the same source.
func TestHandleDocsSearch_HybridRunsBothPaths(t *testing.T) {
	h, s := mustHandler(t)
	h.SetEmbedding(nil, nil, &fixedEmbedProvider{vec: []float32{1, 0, 0, 0}})

	src, err := s.UpsertDocSource(&storage.DocSource{Name: "hybrid-docs", Project: "proj", Version: "1", DocType: "reference"})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	lexical, err := s.InsertDocChunk(&storage.DocChunk{SourceID: src, SourceFile: "a.md", HeadingPath: "Lexical", Content: "goroutine scheduler notes", ContentHash: "hyb-1", TokensApprox: 5})
	if err != nil {
		t.Fatalf("insert lexical chunk: %v", err)
	}
	semantic, err := s.InsertDocChunk(&storage.DocChunk{SourceID: src, SourceFile: "b.md", HeadingPath: "Semantic", Content: "entirely different wording about concurrency", ContentHash: "hyb-2", TokensApprox: 5})
	if err != nil {
		t.Fatalf("insert semantic chunk: %v", err)
	}
	// The lexical hit is a poor vector match, the semantic one a perfect match.
	if err := s.SetDocChunkEmbedding(lexical, []float32{0, 1, 0, 0}, "hyb-1"); err != nil {
		t.Fatalf("embed lexical: %v", err)
	}
	if err := s.SetDocChunkEmbedding(semantic, []float32{1, 0, 0, 0}, "hyb-2"); err != nil {
		t.Fatalf("embed semantic: %v", err)
	}

	res := runDocsSearch(t, h, map[string]any{
		"query":   "goroutine scheduler",
		"project": "proj",
		"limit":   float64(10),
	})
	if res.Method != "hybrid" {
		t.Fatalf("method = %q, want hybrid — the vector path did not run although BM25 had hits", res.Method)
	}
	seen := map[int64]bool{}
	for _, r := range res.Results {
		seen[r.ID] = true
	}
	if !seen[lexical] || !seen[semantic] {
		t.Errorf("hybrid merge dropped a path's top hit: lexical=%v semantic=%v (ids %d/%d)", seen[lexical], seen[semantic], lexical, semantic)
	}
}

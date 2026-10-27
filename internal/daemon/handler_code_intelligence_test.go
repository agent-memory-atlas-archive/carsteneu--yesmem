package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/models"
)

// newTestCodeGraph creates a small graph for handler tests.
func newTestCodeGraph() *codescan.CodeGraph {
	g := codescan.NewCodeGraph()
	g.AddNode(codescan.CodeNode{QualifiedName: "proxy", Kind: "package"})
	g.AddNode(codescan.CodeNode{QualifiedName: "storage", Kind: "package"})
	g.AddNode(codescan.CodeNode{QualifiedName: "proxy.CacheGet", Kind: "function", File: "proxy/cache.go", Signature: "func CacheGet(key string) ([]byte, bool)"})
	g.AddNode(codescan.CodeNode{QualifiedName: "proxy.CacheSet", Kind: "function", File: "proxy/cache.go", Signature: "func CacheSet(key string, val []byte)"})
	g.AddNode(codescan.CodeNode{QualifiedName: "storage.Query", Kind: "function", File: "storage/db.go", Signature: "func Query(sql string) ([]Row, error)"})
	g.AddEdge(codescan.CodeEdge{From: "proxy", To: "storage", Kind: "imports"})
	g.AddEdge(codescan.CodeEdge{From: "proxy", To: "proxy.CacheGet", Kind: "defines"})
	g.AddEdge(codescan.CodeEdge{From: "proxy", To: "proxy.CacheSet", Kind: "defines"})
	g.AddEdge(codescan.CodeEdge{From: "storage", To: "storage.Query", Kind: "defines"})
	// Non-defines edge for caller-count enrichment testing (S9)
	g.AddEdge(codescan.CodeEdge{From: "storage.Query", To: "proxy.CacheGet", Kind: "calls"})
	return g
}

func TestHandleSearchCodeIndex_NoProject(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleSearchCodeIndex(map[string]any{"pattern": "Cache"})
	if resp.Error == "" {
		t.Error("expected error when no project specified")
	}
}

func TestHandleSearchCode_MissingPattern(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleSearchCode(map[string]any{"project": "test"})
	if resp.Error == "" {
		t.Error("expected error when pattern is missing")
	}
}

func TestHandleGetCodeContext_MissingQualifiedName(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleGetCodeContext(map[string]any{"project": "test"})
	if resp.Error == "" {
		t.Error("expected error when qualified_name is missing")
	}
}

func TestHandleGetDependencyMap_MissingPackage(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleGetDependencyMap(map[string]any{"project": "test"})
	if resp.Error == "" {
		t.Error("expected error when package is missing")
	}
}

func TestHandleGraphTraverse_MissingFrom(t *testing.T) {
	h, _ := mustHandler(t)
	resp := h.handleGraphTraverse(map[string]any{"project": "test"})
	if resp.Error == "" {
		t.Error("expected error when from is missing")
	}
}

func TestBoolParam(t *testing.T) {
	params := map[string]any{"flag": true, "off": false}
	if !boolParam(params, "flag", false) {
		t.Error("expected true")
	}
	if boolParam(params, "off", true) {
		t.Error("expected false")
	}
	if !boolParam(params, "missing", true) {
		t.Error("expected default true")
	}
}

func TestIntParamCI(t *testing.T) {
	params := map[string]any{"n": float64(42), "m": 7}
	if intParamCI(params, "n", 0) != 42 {
		t.Error("expected 42 from float64")
	}
	if intParamCI(params, "m", 0) != 7 {
		t.Error("expected 7 from int")
	}
	if intParamCI(params, "missing", 99) != 99 {
		t.Error("expected default 99")
	}
}

func TestTextResponse(t *testing.T) {
	resp := textResponse("hello world")
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	var data map[string]string
	if err := json.Unmarshal(resp.Result, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data["text"] != "hello world" {
		t.Errorf("expected 'hello world', got %q", data["text"])
	}
}

func TestHandleSearchCodeIndex_WithGraph(t *testing.T) {
	h, _ := mustHandler(t)

	h.codeGraphMu.Lock()
	if h.codeGraphs == nil {
		h.codeGraphs = make(map[string]*codeGraphEntry)
	}
	graph := newTestCodeGraph()
	h.codeGraphs["testproject"] = &codeGraphEntry{graph: graph}
	h.codeGraphMu.Unlock()

	resp := h.handleSearchCodeIndex(map[string]any{
		"pattern": "Cache",
		"project": "testproject",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	json.Unmarshal(resp.Result, &data)
	if !strings.Contains(data["text"], "CacheGet") {
		t.Errorf("expected CacheGet in results, got: %s", data["text"])
	}
}

func TestHandleGetCodeContext_WithGraph(t *testing.T) {
	h, _ := mustHandler(t)

	h.codeGraphMu.Lock()
	if h.codeGraphs == nil {
		h.codeGraphs = make(map[string]*codeGraphEntry)
	}
	graph := newTestCodeGraph()
	h.codeGraphs["testproject"] = &codeGraphEntry{graph: graph}
	h.codeGraphMu.Unlock()

	resp := h.handleGetCodeContext(map[string]any{
		"qualified_name":    "proxy.CacheGet",
		"project":           "testproject",
		"include_neighbors": true,
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	json.Unmarshal(resp.Result, &data)
	if !strings.Contains(data["text"], "proxy.CacheGet") {
		t.Errorf("expected proxy.CacheGet in output, got: %s", data["text"])
	}
}

func TestHandleGetDependencyMap_WithGraph(t *testing.T) {
	h, _ := mustHandler(t)

	h.codeGraphMu.Lock()
	if h.codeGraphs == nil {
		h.codeGraphs = make(map[string]*codeGraphEntry)
	}
	graph := newTestCodeGraph()
	h.codeGraphs["testproject"] = &codeGraphEntry{graph: graph}
	h.codeGraphMu.Unlock()

	resp := h.handleGetDependencyMap(map[string]any{
		"package": "proxy",
		"project": "testproject",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	json.Unmarshal(resp.Result, &data)
	if !strings.Contains(data["text"], "storage") {
		t.Errorf("expected storage dependency, got: %s", data["text"])
	}
}

func TestHandleGraphTraverse_WithGraph(t *testing.T) {
	h, _ := mustHandler(t)

	h.codeGraphMu.Lock()
	if h.codeGraphs == nil {
		h.codeGraphs = make(map[string]*codeGraphEntry)
	}
	graph := newTestCodeGraph()
	h.codeGraphs["testproject"] = &codeGraphEntry{graph: graph}
	h.codeGraphMu.Unlock()

	resp := h.handleGraphTraverse(map[string]any{
		"from":      "proxy",
		"direction": "outbound",
		"edge_type": "imports",
		"project":   "testproject",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	json.Unmarshal(resp.Result, &data)
	if !strings.Contains(data["text"], "storage") {
		t.Errorf("expected storage in traversal, got: %s", data["text"])
	}
}

func TestHandleSearchCode_WithGraph(t *testing.T) {
	h, _ := mustHandler(t)

	h.codeGraphMu.Lock()
	if h.codeGraphs == nil {
		h.codeGraphs = make(map[string]*codeGraphEntry)
	}
	graph := newTestCodeGraph()
	h.codeGraphs["testproject"] = &codeGraphEntry{graph: graph}
	h.codeGraphMu.Unlock()

	resp := h.handleSearchCode(map[string]any{
		"pattern": "Cache",
		"project": "testproject",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	json.Unmarshal(resp.Result, &data)
	if !strings.Contains(data["text"], "CacheGet") {
		t.Errorf("expected CacheGet in results, got: %s", data["text"])
	}
	// Verify graph enrichment: CacheGet has 1 caller (storage.Query -> proxy.CacheGet calls edge)
	if !strings.Contains(data["text"], "callers/references") {
		t.Errorf("expected caller count enrichment, got: %s", data["text"])
	}
}

func TestHandleSearchCode_FilePatternFilter(t *testing.T) {
	h, _ := mustHandler(t)

	h.codeGraphMu.Lock()
	if h.codeGraphs == nil {
		h.codeGraphs = make(map[string]*codeGraphEntry)
	}
	graph := newTestCodeGraph()
	h.codeGraphs["testproject"] = &codeGraphEntry{graph: graph}
	h.codeGraphMu.Unlock()

	// Filter to storage files only — should NOT match proxy functions
	resp := h.handleSearchCode(map[string]any{
		"pattern":      "Query",
		"project":      "testproject",
		"file_pattern": "storage",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	json.Unmarshal(resp.Result, &data)
	if !strings.Contains(data["text"], "storage.Query") {
		t.Errorf("expected storage.Query, got: %s", data["text"])
	}
	if strings.Contains(data["text"], "proxy.CacheGet") {
		t.Error("file_pattern filter should exclude proxy functions")
	}
}

func TestHandleSearchCode_RealGrep(t *testing.T) {
	h, s := mustHandler(t)

	// The directory carries the project name: a short name resolves by basename
	// against sessions.project, the form project_short has had since v0.65.
	dir := filepath.Join(t.TempDir(), "greptest")
	os.MkdirAll(filepath.Join(dir, "pkg"), 0755)
	os.WriteFile(filepath.Join(dir, "pkg", "handler.go"), []byte("package pkg\n\nfunc HandleRequest() {\n\tprintln(\"processing request\")\n}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "pkg", "utils.go"), []byte("package pkg\n\nfunc FormatRequest(r string) string {\n\treturn r\n}\n"), 0644)

	// Register project in DB so ResolveProjectPath works
	_, err2 := s.DB().Exec("INSERT INTO sessions (id, project, project_short, started_at, message_count, jsonl_path, indexed_at) VALUES (?, ?, ?, datetime('now'), 1, '', datetime('now'))", "test-sess", dir, dir)
	if err2 != nil {
		t.Fatalf("insert session: %v", err2)
	}

	// Build graph from the temp dir using DirectoryScanner (no external dependency)
	scanner := &codescan.DirectoryScanner{}
	result, err := scanner.Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	graph := codescan.BuildCodeGraph(result)

	h.codeGraphMu.Lock()
	if h.codeGraphs == nil {
		h.codeGraphs = make(map[string]*codeGraphEntry)
	}
	h.codeGraphs[dir] = &codeGraphEntry{graph: graph}
	h.codeGraphMu.Unlock()

	// Search for "request" — should grep actual files
	resp := h.handleSearchCode(map[string]any{
		"pattern": "request",
		"project": "greptest",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	json.Unmarshal(resp.Result, &data)

	// Should find matches in actual file content (grep, not just graph names)
	if !strings.Contains(data["text"], "processing request") {
		t.Errorf("should grep file content and find 'processing request', got:\n%s", data["text"])
	}
	// Must NOT be the graph-only fallback
	if strings.Contains(data["text"], "graph-only") {
		t.Errorf("should use real grep path, not graph-only fallback, got:\n%s", data["text"])
	}
	// Should show line numbers
	if !strings.Contains(data["text"], "L") {
		t.Errorf("should show line numbers, got:\n%s", data["text"])
	}
}

func TestHandleGetFileIndex(t *testing.T) {
	h, s := mustHandler(t)

	// The directory carries the project name: a short name resolves by basename
	// against sessions.project, the form project_short has had since v0.65.
	dir := filepath.Join(t.TempDir(), "filetest")
	os.MkdirAll(filepath.Join(dir, "pkg"), 0755)
	os.WriteFile(filepath.Join(dir, "pkg", "handler.go"), []byte("package pkg\n\nfunc Handle() {}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "pkg", "utils.go"), []byte("package pkg\n\nfunc Util() {}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644)

	// Register project in DB
	_, err := s.DB().Exec("INSERT INTO sessions (id, project, project_short, started_at, message_count, jsonl_path, indexed_at) VALUES (?, ?, ?, datetime('now'), 1, '', datetime('now'))", "test-sess", dir, dir)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Add a learning with entity matching a file
	l := &models.Learning{Category: "gotcha", Content: "handler bug", Project: "filetest", Entities: []string{"handler.go"}, CreatedAt: time.Now(), Source: "test", ModelUsed: "test"}
	s.InsertLearning(l)

	resp := h.handleGetFileIndex(map[string]any{
		"dir":     "pkg",
		"project": "filetest",
	})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	json.Unmarshal(resp.Result, &data)

	// Should list Go source files
	if !strings.Contains(data["text"], "handler.go") {
		t.Errorf("should list handler.go, got:\n%s", data["text"])
	}
	if !strings.Contains(data["text"], "utils.go") {
		t.Errorf("should list utils.go, got:\n%s", data["text"])
	}
	// Should show learning annotation for handler.go
	if !strings.Contains(data["text"], "gotcha") {
		t.Errorf("should show gotcha annotation for handler.go, got:\n%s", data["text"])
	}
}

// errScanner fails every scan, so the failure paths are testable without the
// CBM binary (CI has none installed — see the integration tests' skip guards).
type errScanner struct{}

func (errScanner) Scan(string) (*codescan.ScanResult, error) {
	return nil, errors.New("scan unavailable")
}

// After HEAD moves the cache must serve a freshly built graph, not the object
// it held before (DT26 / YM-5). The unconditional cache lookup of origin/main
// (and of the first version of this branch) fails this assertion by serving the
// object from before the move.
func TestGetCodeGraph_RebuildsAfterHeadMove(t *testing.T) {
	h, s := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }
	dir := initGitRepo(t)

	// Seed a stored scan so the rebuild needs no live index either. The key is
	// the checkout root — the one the scanner writes and the handler reads
	// (codescan.ScanCacheKey, YM-453).
	scanJSON := `{"RootDir":"` + dir + `","Packages":[{"Files":[{"Path":"a.go"}]}]}`
	if err := s.PersistScan(dir, scanJSON, "", 0); err != nil {
		t.Fatalf("persist scan: %v", err)
	}

	first := h.getCodeGraph(dir)
	if first == nil {
		t.Fatal("first build returned no graph")
	}

	// Move HEAD: the graph built from the old commit is stale.
	head, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(head)), "ref: "))
	if err := os.MkdirAll(filepath.Join(dir, ".git", filepath.Dir(ref)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", ref), []byte("movedhead123\n"), 0644); err != nil {
		t.Fatalf("write ref: %v", err)
	}

	second := h.getCodeGraph(dir)
	if second == nil {
		t.Fatal("rebuild after the HEAD move returned no graph")
	}
	if second == first {
		t.Fatal("the graph cached before the HEAD move was served again")
	}

	h.codeGraphMu.RLock()
	entry := h.codeGraphs[dir]
	h.codeGraphMu.RUnlock()
	if entry == nil || entry.head != "movedhead123" {
		t.Fatalf("cache entry must record the new HEAD, got %+v", entry)
	}
}

// A failed rebuild must not turn into an outage: the last known graph is served
// instead of nothing (review finding I-1 — one CBM hiccup plus a cooldown used
// to lock every code tool out for ten minutes).
func TestGetCodeGraph_FallsBackToStaleGraphOnScanFailure(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return errScanner{} }
	dir := t.TempDir()
	known := newTestCodeGraph()

	h.codeGraphMu.Lock()
	h.codeGraphs = map[string]*codeGraphEntry{dir: {graph: known, head: "old-head"}}
	h.codeGraphMu.Unlock()

	if got := h.getCodeGraph(dir); got != known {
		t.Fatal("a failed rebuild must fall back to the cached graph")
	}
}

// initGitRepo creates a repository so ReadGitHead yields a real cache key.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// The cache key is the project's git HEAD plus CBM index mtime: an entry that
// still matches is fresh, one whose HEAD moved is stale (DT26 / YM-5).
func TestCodeGraphFresh(t *testing.T) {
	dir := initGitRepo(t)
	head := codescan.ReadGitHead(dir)
	if head == "" {
		t.Fatal("fixture broken: no git HEAD")
	}
	entry := &codeGraphEntry{
		graph:    newTestCodeGraph(),
		head:     head,
		cbmMtime: codescan.CBMIndexMtime(dir).Unix(),
	}
	if !codeGraphFresh(entry, dir) {
		t.Fatal("an entry built from the current HEAD must be fresh")
	}

	entry.head = "deadbeef"
	if codeGraphFresh(entry, dir) {
		t.Fatal("an entry whose HEAD moved must be stale")
	}

	// A keyed entry cannot be verified without a directory to compare against.
	entry.head = head
	if codeGraphFresh(entry, "") {
		t.Fatal("a keyed entry without a directory must not count as fresh")
	}
}

// A fresh entry is served as-is — freshness tracking must not turn every call
// into a rebuild.
func TestGetCodeGraph_ServesFreshEntry(t *testing.T) {
	h, _ := mustHandler(t)
	dir := initGitRepo(t)
	cached := newTestCodeGraph()

	h.codeGraphMu.Lock()
	h.codeGraphs = map[string]*codeGraphEntry{dir: {
		graph:    cached,
		head:     codescan.ReadGitHead(dir),
		cbmMtime: codescan.CBMIndexMtime(dir).Unix(),
	}}
	h.codeGraphMu.Unlock()

	if got := h.getCodeGraph(dir); got != cached {
		t.Fatal("a fresh cache entry must be served without a rebuild")
	}
}

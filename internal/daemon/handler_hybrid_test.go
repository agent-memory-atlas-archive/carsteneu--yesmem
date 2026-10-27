package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// mustHandlerWithEmbedding creates a test handler with embedding_vector column
// (missing in initial schema, added only via migration v0.26).
func mustHandlerWithEmbedding(t *testing.T) (*Handler, *storage.Store) {
	t.Helper()
	h, s := mustHandler(t)
	s.DB().Exec(`ALTER TABLE learnings ADD COLUMN embedding_vector BLOB`)
	return h, s
}

func TestHandleHybridSearch(t *testing.T) {
	h, s := mustHandlerWithEmbedding(t)

	// Insert learnings so VectorStore.Add() can UPDATE them
	s.DB().Exec(`INSERT INTO learnings(id, content, category) VALUES (1, 'nginx reverse proxy configuration', 'test')`)
	s.DB().Exec(`INSERT INTO learnings(id, content, category) VALUES (2, 'docker compose networking setup', 'test')`)

	store, err := embedding.NewVectorStore(s.DB(), 384)
	if err != nil {
		t.Fatal(err)
	}

	provider := &testEmbedProvider{dims: 384}
	indexer := embedding.NewIndexer(provider, store)
	h.SetEmbedding(indexer, store, provider)

	ctx := context.Background()
	indexer.Index(ctx, "1", "nginx reverse proxy configuration", nil)
	indexer.Index(ctx, "2", "docker compose networking setup", nil)

	resp := h.Handle(Request{
		Method: "hybrid_search",
		Params: map[string]any{
			"query": "nginx proxy",
			"limit": float64(5),
		},
	})

	if resp.Error != "" {
		t.Fatalf("hybrid_search returned error: %s", resp.Error)
	}
	if len(resp.Result) == 0 {
		t.Fatal("expected non-empty result")
	}
}

func TestHandleHybridSearchWithoutVectorStore(t *testing.T) {
	h, _ := mustHandler(t)
	// No SetEmbedding called — graceful degradation to BM25 only

	resp := h.Handle(Request{
		Method: "hybrid_search",
		Params: map[string]any{
			"query": "test",
			"limit": float64(5),
		},
	})

	if resp.Error != "" {
		t.Fatalf("hybrid_search without vector store should not error: %s", resp.Error)
	}
}

// testEmbedProvider for daemon tests
type testEmbedProvider struct {
	dims int
}

func (m *testEmbedProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, m.dims)
		for j := range v {
			v[j] = float32(len(text))*0.01 + float32(j)*0.001
		}
		var norm float32
		for _, x := range v {
			norm += x * x
		}
		norm = 1.0 / norm
		for j := range v {
			v[j] *= norm
		}
		vecs[i] = v
	}
	return vecs, nil
}

func (m *testEmbedProvider) Dimensions() int { return m.dims }
func (m *testEmbedProvider) Enabled() bool   { return true }
func (m *testEmbedProvider) Close() error    { return nil }

// --- Project-Recency Boost Unit Tests ---

func TestApplyTurnBasedDecay_RecentVsOld(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().Format("2006-01-02 15:04:05")

	// Learning created at turn 90 (recent) vs turn 0 (old), current turn = 100
	s.DB().Exec(`INSERT INTO learnings(id, content, category, project, created_at, model_used, source, turns_at_creation) VALUES (1, 'old pattern', 'pattern', 'memory', ?, 'test', 'llm_extracted', 0)`, now)
	s.DB().Exec(`INSERT INTO learnings(id, content, category, project, created_at, model_used, source, turns_at_creation) VALUES (2, 'recent pattern', 'pattern', 'memory', ?, 'test', 'llm_extracted', 90)`, now)
	s.DB().Exec(`INSERT OR REPLACE INTO turn_counters(project, turn_count) VALUES ('memory', 100)`)

	results := []embedding.RankedResult{
		{ID: "1", Content: "old pattern", Score: 0.5, Project: "memory"},
		{ID: "2", Content: "recent pattern", Score: 0.5, Project: "memory"},
	}

	decayed := h.applyTurnBasedDecay(results, "memory")

	// Recent (10 turns since) should rank higher than old (100 turns since)
	if decayed[0].ID != "2" {
		t.Errorf("recent learning should rank first, got id=%s", decayed[0].ID)
	}
	if decayed[0].Score >= 0.5 {
		t.Errorf("recent score should be decayed below 0.5, got %.4f", decayed[0].Score)
	}
	if decayed[0].Score <= decayed[1].Score {
		t.Errorf("recent should score higher than old: recent=%.4f old=%.4f", decayed[0].Score, decayed[1].Score)
	}
}

func TestApplyTurnBasedDecay_EmptyResults(t *testing.T) {
	h, _ := mustHandler(t)

	results := []embedding.RankedResult{}
	decayed := h.applyTurnBasedDecay(results, "memory")

	if len(decayed) != 0 {
		t.Errorf("empty input should return empty output, got %d", len(decayed))
	}
}

func TestApplyTurnBasedDecay_NoTurnCounter(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().Format("2006-01-02 15:04:05")
	s.DB().Exec(`INSERT INTO learnings(id, content, category, project, created_at, model_used, source, turns_at_creation) VALUES (1, 'pattern', 'pattern', 'memory', ?, 'test', 'llm_extracted', 0)`, now)

	results := []embedding.RankedResult{
		{ID: "1", Content: "pattern", Score: 0.5, Project: "memory"},
	}

	// No turn_counters row → currentTurnCount=0, turnsSince=0 → decay=1.0
	decayed := h.applyTurnBasedDecay(results, "memory")

	if decayed[0].Score != 0.5 {
		t.Errorf("no turn counter should mean no decay: got %.4f, want 0.5", decayed[0].Score)
	}
}

func TestApplyTurnBasedDecay_UserStatedFloor(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().Format("2006-01-02 15:04:05")
	s.DB().Exec(`INSERT INTO learnings(id, content, category, project, created_at, model_used, source, turns_at_creation) VALUES (1, 'user rule', 'preference', 'memory', ?, 'test', 'user_stated', 0)`, now)
	s.DB().Exec(`INSERT INTO learnings(id, content, category, project, created_at, model_used, source, turns_at_creation) VALUES (2, 'extracted rule', 'preference', 'memory', ?, 'test', 'llm_extracted', 0)`, now)
	s.DB().Exec(`INSERT OR REPLACE INTO turn_counters(project, turn_count) VALUES ('memory', 10000)`)

	results := []embedding.RankedResult{
		{ID: "1", Content: "user rule", Score: 1.0, Project: "memory"},
		{ID: "2", Content: "extracted rule", Score: 1.0, Project: "memory"},
	}

	decayed := h.applyTurnBasedDecay(results, "memory")

	// user_stated should have floor of 0.5, llm_extracted floor of 0.1
	if decayed[0].ID != "1" {
		t.Errorf("user_stated should rank first due to higher floor, got id=%s", decayed[0].ID)
	}
	if decayed[0].Score < 0.49 {
		t.Errorf("user_stated floor should be >= 0.5, got %.4f", decayed[0].Score)
	}
}

func TestGraphAugmentation(t *testing.T) {
	h, s := mustHandler(t)

	l1 := &models.Learning{Content: "proxy config needs restart after change", Category: "gotcha", Source: "user_stated", CreatedAt: time.Now().Add(-2 * time.Hour)}
	id1, _ := s.InsertLearning(l1)
	s.DB().Exec("UPDATE learnings SET created_at = datetime('now', '-2 hours') WHERE id = ?", id1)

	l2 := &models.Learning{Content: "config reload requires daemon restart too", Category: "gotcha", Source: "user_stated", CreatedAt: time.Now().Add(-2 * time.Hour)}
	id2, _ := s.InsertLearning(l2)
	s.DB().Exec("UPDATE learnings SET created_at = datetime('now', '-2 hours') WHERE id = ?", id2)

	s.InsertTypedAssociation(id1, id2, "depends_on")

	h.EmbedLearning(id1, l1.Content, l1.Category, "")
	h.EmbedLearning(id2, l2.Content, l2.Category, "")

	resp := h.Handle(Request{
		Method: "hybrid_search",
		Params: map[string]any{"query": "proxy config restart", "limit": float64(10)},
	})
	if resp.Error != "" {
		t.Fatalf("hybrid_search error: %s", resp.Error)
	}

	var result struct {
		Results []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"results"`
		MergedCount int `json:"merged_count"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}

	foundGraph := false
	for _, r := range result.Results {
		if r.ID == fmt.Sprintf("%d", id2) && strings.HasPrefix(r.Source, "graph") {
			foundGraph = true
		}
	}
	if !foundGraph {
		t.Error("expected depends_on neighbor to appear with graph source")
	}
	t.Logf("merged_count=%d, foundGraph=%v", result.MergedCount, foundGraph)
}

func TestAugmentWithGraphNeighbors_DependsOnHigherScore(t *testing.T) {
	h, s := mustHandler(t)

	l1 := &models.Learning{Content: "base config", Category: "pattern", Source: "user_stated", CreatedAt: time.Now().Add(-1 * time.Hour)}
	id1, _ := s.InsertLearning(l1)

	l2 := &models.Learning{Content: "prerequisite config", Category: "pattern", Source: "user_stated", CreatedAt: time.Now().Add(-1 * time.Hour)}
	id2, _ := s.InsertLearning(l2)

	l3 := &models.Learning{Content: "loosely related info", Category: "pattern", Source: "user_stated", CreatedAt: time.Now().Add(-1 * time.Hour)}
	id3, _ := s.InsertLearning(l3)

	s.InsertTypedAssociation(id1, id2, "depends_on")
	s.InsertTypedAssociation(id1, id3, "relates_to")

	merged := []embedding.RankedResult{
		{ID: fmt.Sprintf("%d", id1), Content: "base config", Score: 50.0, Source: "keyword"},
	}

	result := h.augmentWithGraphNeighbors(merged)

	var dependsOnScore, relatesToScore float64
	var dependsOnSource, relatesToSource string
	for _, r := range result {
		switch r.ID {
		case fmt.Sprintf("%d", id2):
			dependsOnScore = r.Score
			dependsOnSource = r.Source
		case fmt.Sprintf("%d", id3):
			relatesToScore = r.Score
			relatesToSource = r.Source
		}
	}

	if dependsOnScore == 0 {
		t.Fatal("depends_on neighbor not found in results")
	}
	if relatesToScore == 0 {
		t.Fatal("relates_to neighbor not found in results")
	}

	// depends_on should score higher than relates_to (same weight=1.0)
	if dependsOnScore <= relatesToScore {
		t.Errorf("depends_on should score higher: depends_on=%.2f, relates_to=%.2f",
			dependsOnScore, relatesToScore)
	}

	// Source should reflect the relation type
	if dependsOnSource != "graph:depends_on" {
		t.Errorf("expected source 'graph:depends_on', got %q", dependsOnSource)
	}
	if relatesToSource != "graph" {
		t.Errorf("expected source 'graph' for relates_to, got %q", relatesToSource)
	}
}

func TestAugmentWithGraphNeighbors_SupportsSource(t *testing.T) {
	h, s := mustHandler(t)

	l1 := &models.Learning{Content: "main claim", Category: "decision", Source: "user_stated", CreatedAt: time.Now().Add(-1 * time.Hour)}
	id1, _ := s.InsertLearning(l1)

	l2 := &models.Learning{Content: "supporting evidence", Category: "decision", Source: "user_stated", CreatedAt: time.Now().Add(-1 * time.Hour)}
	id2, _ := s.InsertLearning(l2)

	s.InsertTypedAssociation(id1, id2, "supports")

	merged := []embedding.RankedResult{
		{ID: fmt.Sprintf("%d", id1), Content: "main claim", Score: 40.0, Source: "keyword"},
	}

	result := h.augmentWithGraphNeighbors(merged)

	for _, r := range result {
		if r.ID == fmt.Sprintf("%d", id2) {
			if r.Source != "graph:supports" {
				t.Errorf("expected source 'graph:supports', got %q", r.Source)
			}
			return
		}
	}
	t.Error("supports neighbor not found in results")
}

func TestResolveSupersededResults(t *testing.T) {
	h, s := mustHandler(t)

	l1 := &models.Learning{Content: "original", Category: "decision", Source: "user_stated"}
	id1, _ := s.InsertLearning(l1)

	l2 := &models.Learning{Content: "replacement", Category: "decision", Source: "user_stated"}
	id2, _ := s.InsertLearning(l2)

	s.SupersedeLearning(id1, id2, "newer approach")

	results := []embedding.RankedResult{
		{ID: fmt.Sprintf("%d", id1), Score: 0.9},
		{ID: fmt.Sprintf("%d", id2), Score: 0.8},
	}

	resolved := h.resolveSupersededResults(results)

	// Superseded result should be replaced by its successor
	for _, r := range resolved {
		if r.ID == fmt.Sprintf("%d", id1) {
			t.Error("superseded learning should not appear in results")
		}
	}
}

// TestResolveSupersededResults_DropsResolvedChainEnd guards against promoting a
// learning that is itself inactive: X was superseded by Y, Y was later resolved
// (superseded_by = -2). Y must not surface as X's "active successor".
func TestResolveSupersededResults_DropsResolvedChainEnd(t *testing.T) {
	h, s := mustHandler(t)

	l1 := &models.Learning{Content: "old decision text", Category: "decision", Source: "user_stated"}
	id1, err := s.InsertLearning(l1)
	if err != nil {
		t.Fatalf("insert l1: %v", err)
	}
	l2 := &models.Learning{Content: "middle decision text", Category: "decision", Source: "user_stated"}
	id2, err := s.InsertLearning(l2)
	if err != nil {
		t.Fatalf("insert l2: %v", err)
	}

	if err := s.SupersedeLearning(id1, id2, "newer approach"); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if err := s.ResolveLearning(id2, "resolved later"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	results := []embedding.RankedResult{
		{ID: fmt.Sprintf("%d", id1), Score: 0.9},
		{ID: fmt.Sprintf("%d", id2), Score: 0.8},
	}

	resolved := h.resolveSupersededResults(results)
	for _, r := range resolved {
		if r.ID == fmt.Sprintf("%d", id2) {
			t.Fatalf("resolved learning #%d was promoted as an active successor", id2)
		}
	}
}

func TestEnrichRankedResults_Empty(t *testing.T) {
	h, _ := mustHandler(t)
	enriched := h.enrichRankedResults(nil)
	if len(enriched) != 0 {
		t.Errorf("expected empty, got %d", len(enriched))
	}
}

// --- YM-38 / YM-39 / YM-41 / YM-44 regression tests ---

// insertAgedLearning inserts a learning with a UTC created_at well outside the
// 30 minute cooldown. UTC matters: the cooldown comparison is only meaningful for
// timestamps sharing one zone representation.
func insertAgedLearning(t *testing.T, s *storage.Store, l *models.Learning) int64 {
	t.Helper()
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now().Add(-2 * time.Hour).UTC()
	}
	id, err := s.InsertLearning(l)
	if err != nil {
		t.Fatalf("insert learning %q: %v", l.Content, err)
	}
	return id
}

// mustVectorHandler wires a vector store with a deterministic test provider.
func mustVectorHandler(t *testing.T) (*Handler, *storage.Store, *embedding.Indexer) {
	t.Helper()
	h, s := mustHandlerWithEmbedding(t)
	store, err := embedding.NewVectorStore(s.DB(), 4)
	if err != nil {
		t.Fatalf("new vector store: %v", err)
	}
	provider := &testEmbedProvider{dims: 4}
	indexer := embedding.NewIndexer(provider, store)
	h.SetEmbedding(indexer, store, provider)
	return h, s, indexer
}

// hybridSearchRun executes hybrid_search and returns id → project of the results.
func hybridSearchRun(t *testing.T, h *Handler, query string) map[string]string {
	t.Helper()
	resp := h.Handle(Request{
		Method: "hybrid_search",
		Params: map[string]any{"query": query, "limit": float64(20)},
	})
	if resp.Error != "" {
		t.Fatalf("hybrid_search error: %s", resp.Error)
	}
	var out struct {
		Results []struct {
			ID      string `json:"id"`
			Project string `json:"project"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("parse hybrid_search result: %v", err)
	}
	byID := make(map[string]string, len(out.Results))
	for _, r := range out.Results {
		byID[r.ID] = r.Project
	}
	return byID
}

// TestHybridSearchVectorHitCarriesItsProject (YM-38): a hit that only the vector
// lane found must carry the learning's project. Project-less hits fall through the
// proxy's fallback gate and are dropped there ([SKIP-noproj-fallback]).
func TestHybridSearchVectorHitCarriesItsProject(t *testing.T) {
	h, s, indexer := mustVectorHandler(t)

	id := insertAgedLearning(t, s, &models.Learning{
		Content:  "ingress tls termination sentinel",
		Category: "pattern",
		Project:  "proj-alpha",
		Source:   "llm_extracted",
	})
	if err := indexer.Index(context.Background(), fmt.Sprintf("%d", id), "ingress tls termination sentinel", nil); err != nil {
		t.Fatalf("index: %v", err)
	}

	// Query without any FTS overlap: only the vector lane can produce this hit.
	results := hybridSearchRun(t, h, "zzz qqq")

	project, ok := results[fmt.Sprintf("%d", id)]
	if !ok {
		t.Fatalf("vector hit %d missing from results: %v", id, results)
	}
	if project != "proj-alpha" {
		t.Errorf("vector hit lost its project: got %q, want %q", project, "proj-alpha")
	}
}

// TestHybridSearchCooldownParsesTimestampsWithOffset (YM-39): created_at carries
// the writer's UTC offset (fmtTime writes RFC3339 in local time). Comparing that
// string against a UTC cutoff made the 30 minute cooldown last 2.5 hours.
func TestHybridSearchCooldownParsesTimestampsWithOffset(t *testing.T) {
	h, s, indexer := mustVectorHandler(t)

	id := insertAgedLearning(t, s, &models.Learning{
		Content:  "cooldown offset sentinel",
		Category: "pattern",
		Source:   "llm_extracted",
	})
	// 90 minutes old, written in +02:00: far outside the cooldown, but
	// lexicographically greater than a UTC "now minus 30 minutes" cutoff.
	createdAt := time.Now().Add(-90 * time.Minute).In(time.FixedZone("CEST", 2*3600)).Format(time.RFC3339)
	if _, err := s.DB().Exec("UPDATE learnings SET created_at = ? WHERE id = ?", createdAt, id); err != nil {
		t.Fatalf("set created_at: %v", err)
	}
	if err := indexer.Index(context.Background(), fmt.Sprintf("%d", id), "cooldown offset sentinel", nil); err != nil {
		t.Fatalf("index: %v", err)
	}

	results := hybridSearchRun(t, h, "zzz qqq")

	if _, ok := results[fmt.Sprintf("%d", id)]; !ok {
		t.Errorf("learning created 90 minutes ago was suppressed as if it were inside the cooldown (%s): %v", createdAt, results)
	}
}

// TestHybridSearchGraphNeighborRespectsCategoryFilter (YM-41): neighbours pulled in
// through association edges must pass the same category filter as search hits.
func TestHybridSearchGraphNeighborRespectsCategoryFilter(t *testing.T) {
	h, s, indexer := mustVectorHandler(t)

	parentID := insertAgedLearning(t, s, &models.Learning{
		Content:  "parent anchor about proxy restart",
		Category: "gotcha",
		Source:   "user_stated",
	})
	neighborID := insertAgedLearning(t, s, &models.Learning{
		Content:  "narrative summary that must never be injected",
		Category: "narrative",
		Source:   "llm_extracted",
	})
	if err := s.InsertTypedAssociation(parentID, neighborID, "relates_to"); err != nil {
		t.Fatalf("insert association: %v", err)
	}
	if err := indexer.Index(context.Background(), fmt.Sprintf("%d", parentID), "parent anchor about proxy restart", nil); err != nil {
		t.Fatalf("index: %v", err)
	}

	results := hybridSearchRun(t, h, "zzz qqq")

	if _, ok := results[fmt.Sprintf("%d", parentID)]; !ok {
		t.Fatalf("parent anchor missing from results: %v", results)
	}
	if _, ok := results[fmt.Sprintf("%d", neighborID)]; ok {
		t.Errorf("narrative learning %d was injected as a graph neighbor, bypassing the category filter", neighborID)
	}
}

// TestHybridSearchGraphNeighborRespectsCooldown (YM-41): neighbours must respect
// the 30 minute cooldown just like search hits.
func TestHybridSearchGraphNeighborRespectsCooldown(t *testing.T) {
	h, s, indexer := mustVectorHandler(t)

	parentID := insertAgedLearning(t, s, &models.Learning{
		Content:  "parent anchor about docker networking",
		Category: "gotcha",
		Source:   "user_stated",
	})
	neighborID := insertAgedLearning(t, s, &models.Learning{
		Content:   "just written neighbour learning",
		Category:  "gotcha",
		Source:    "llm_extracted",
		CreatedAt: time.Now().Add(-time.Minute).UTC(),
	})
	if err := s.InsertTypedAssociation(parentID, neighborID, "supports"); err != nil {
		t.Fatalf("insert association: %v", err)
	}
	if err := indexer.Index(context.Background(), fmt.Sprintf("%d", parentID), "parent anchor about docker networking", nil); err != nil {
		t.Fatalf("index: %v", err)
	}

	results := hybridSearchRun(t, h, "zzz qqq")

	if _, ok := results[fmt.Sprintf("%d", parentID)]; !ok {
		t.Fatalf("parent anchor missing from results: %v", results)
	}
	if _, ok := results[fmt.Sprintf("%d", neighborID)]; ok {
		t.Errorf("learning %d created one minute ago was injected as a graph neighbor, bypassing the cooldown", neighborID)
	}
}

// TestHybridSearchGraphNeighborOfFilteredParentIsNotAdded (YM-41): expanding from a
// hit that the category filter drops would smuggle its neighbour past the same gate.
func TestHybridSearchGraphNeighborOfFilteredParentIsNotAdded(t *testing.T) {
	h, s, indexer := mustVectorHandler(t)

	parentID := insertAgedLearning(t, s, &models.Learning{
		Content:  "narrative parent that the filter drops",
		Category: "narrative",
		Source:   "llm_extracted",
	})
	neighborID := insertAgedLearning(t, s, &models.Learning{
		Content:  "gotcha neighbour of a dropped parent",
		Category: "gotcha",
		Source:   "llm_extracted",
	})
	if err := s.InsertTypedAssociation(parentID, neighborID, "supports"); err != nil {
		t.Fatalf("insert association: %v", err)
	}
	if err := indexer.Index(context.Background(), fmt.Sprintf("%d", parentID), "narrative parent that the filter drops", nil); err != nil {
		t.Fatalf("index: %v", err)
	}

	results := hybridSearchRun(t, h, "zzz qqq")

	if _, ok := results[fmt.Sprintf("%d", parentID)]; ok {
		t.Fatalf("narrative parent %d should be filtered", parentID)
	}
	if _, ok := results[fmt.Sprintf("%d", neighborID)]; ok {
		t.Errorf("neighbour %d of a filtered parent was injected", neighborID)
	}
}

// TestAugmentWithGraphNeighborsSkipsContradictingEdges (YM-41): a contradiction is
// surfaced separately (proxy contradiction warning), never as ordinary context.
func TestAugmentWithGraphNeighborsSkipsContradictingEdges(t *testing.T) {
	h, s := mustHandler(t)

	id1 := insertAgedLearning(t, s, &models.Learning{
		Content:  "decision that will be contradicted",
		Category: "decision",
		Source:   "user_stated",
	})
	id2 := insertAgedLearning(t, s, &models.Learning{
		Content:  "opposing decision text",
		Category: "decision",
		Source:   "user_stated",
	})
	if err := s.InsertTypedAssociation(id1, id2, "contradicts"); err != nil {
		t.Fatalf("insert association: %v", err)
	}

	result := h.augmentWithGraphNeighbors([]embedding.RankedResult{
		{ID: fmt.Sprintf("%d", id1), Content: "decision that will be contradicted", Score: 50.0, Source: "keyword"},
	})

	for _, r := range result {
		if r.ID == fmt.Sprintf("%d", id2) {
			t.Errorf("contradicts edge was expanded as a normal neighbour (source=%q)", r.Source)
		}
	}
}

// TestAugmentWithGraphNeighborsAppliesUnfinishedDampening (YM-462): the neighbor
// path ran the narrative filter, the cooldown, the source boost and the
// contradicts exclusion, but not the unfinished dampening the direct path applies
// at the metadata stage. A neighbor of the same learning therefore carried a
// higher score than the learning itself would have had as a direct hit. Both
// paths now go through applyLearningGates, so the 0.7 factor holds for a
// neighbor as well.
func TestAugmentWithGraphNeighborsAppliesUnfinishedDampening(t *testing.T) {
	h, s := mustHandler(t)

	parent := insertAgedLearning(t, s, &models.Learning{
		Content:  "parent decision for the gate check",
		Category: "decision",
		Source:   "llm_extracted",
	})
	finished := insertAgedLearning(t, s, &models.Learning{
		Content:  "related gotcha, finished",
		Category: "gotcha",
		Source:   "llm_extracted",
	})
	unfinished := insertAgedLearning(t, s, &models.Learning{
		Content:  "related open thread, unfinished",
		Category: "unfinished",
		Source:   "llm_extracted",
	})
	for _, target := range []int64{finished, unfinished} {
		if err := s.InsertTypedAssociation(parent, target, "relates_to"); err != nil {
			t.Fatalf("insert association to %d: %v", target, err)
		}
	}

	result := h.augmentWithGraphNeighbors([]embedding.RankedResult{
		{ID: fmt.Sprintf("%d", parent), Content: "parent decision for the gate check", Score: 100, Source: "keyword"},
	})

	score := map[string]float64{}
	for _, r := range result {
		score[r.ID] = r.Score
	}
	got, want := fmt.Sprintf("%d", unfinished), fmt.Sprintf("%d", finished)
	if score[got] == 0 || score[want] == 0 {
		t.Fatalf("both neighbors must be expanded, got %v", score)
	}
	if ratio := score[got] / score[want]; math.Abs(ratio-0.7) > 0.001 {
		t.Errorf("the unfinished neighbor carries %.3f of the finished neighbor's score, want 0.7 — the dampening the direct path applies is missing on this path (unfinished=%.3f finished=%.3f)",
			ratio, score[got], score[want])
	}
}

// TestApplyLearningGates_UnfinishedDampingAndSourceBoost guards the two
// adjustments the direct path carried in three separate loops before the merge:
// the unfinished dampening must reach the score, and the source boost must too.
// The multiplication commutes, so this cannot pin their order — what it pins is
// that neither factor disappears and that the four drop rules keep dropping. The
// neighbour test alone would leave every direct hit's score unguarded, which is
// user-visible.
func TestApplyLearningGates_UnfinishedDampingAndSourceBoost(t *testing.T) {
	gated, keep := applyLearningGates(
		embedding.RankedResult{ID: "1", Score: 100},
		learningGates{category: "unfinished", source: "user_stated", createdAt: time.Now().Add(-2 * time.Hour)},
		"",
	)
	if !keep {
		t.Fatal("an unfinished hit outside the cooldown must stay in the result set")
	}
	if want := 100 * 0.7 * 1.25; math.Abs(gated.Score-want) > 0.001 {
		t.Errorf("score = %v, want %v (0.7 unfinished dampening times the 1.25 user_stated boost)", gated.Score, want)
	}

	drops := []struct {
		name string
		g    learningGates
		rel  string
	}{
		{"a narrative row", learningGates{category: "narrative"}, ""},
		{"a quarantined row", learningGates{quarantined: true}, ""},
		{"a contradicts edge", learningGates{category: "decision"}, "contradicts"},
		{"a hit inside the result cooldown", learningGates{createdAt: time.Now()}, ""},
	}
	for _, d := range drops {
		if _, keep := applyLearningGates(embedding.RankedResult{ID: "1", Score: 10}, d.g, d.rel); keep {
			t.Errorf("%s must be dropped", d.name)
		}
	}
}

// TestHybridSearchVectorLaneExcludesQuarantined (YM-44): the vector SELECTs filter
// superseded_by but not quarantined_at, so quarantined learnings came back through
// the semantic lane.
func TestHybridSearchVectorLaneExcludesQuarantined(t *testing.T) {
	h, s, indexer := mustVectorHandler(t)

	id := insertAgedLearning(t, s, &models.Learning{
		Content:   "quarantined vector sentinel",
		Category:  "gotcha",
		SessionID: "sess-quar-vector",
		Source:    "llm_extracted",
	})
	if err := indexer.Index(context.Background(), fmt.Sprintf("%d", id), "quarantined vector sentinel", nil); err != nil {
		t.Fatalf("index: %v", err)
	}
	affected, err := s.QuarantineSession("sess-quar-vector")
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if affected != 1 {
		t.Fatalf("quarantine affected %d rows, want 1", affected)
	}

	results := hybridSearchRun(t, h, "zzz qqq")

	if _, ok := results[fmt.Sprintf("%d", id)]; ok {
		t.Errorf("quarantined learning %d was returned through the vector lane", id)
	}
}

// TestHybridSearchDropsQuarantinedMetadataHit (YM-44): the bulk metadata block in
// handleHybridSearch reads category/source without quarantined_at, so quarantined
// rows that entered the merged set anyway (here: an anticipated-query vector that
// carries its parent's learning_id) were still injected.
func TestHybridSearchDropsQuarantinedMetadataHit(t *testing.T) {
	h, s, _ := mustVectorHandler(t)

	id := insertAgedLearning(t, s, &models.Learning{
		Content:   "quarantined anticipation sentinel",
		Category:  "gotcha",
		SessionID: "sess-quarantine-meta",
		Source:    "llm_extracted",
	})
	if _, err := s.QuarantineSession("sess-quarantine-meta"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	// AQ vectors live in memory and resolve to the parent learning via learning_id;
	// no SQL filter runs on them, so the metadata block is the last gate.
	if err := h.VectorStore().AddBatch(context.Background(), []embedding.VectorDoc{{
		ID:        fmt.Sprintf("%d_aq_0", id),
		Embedding: make([]float32, 4),
		Metadata: map[string]string{
			"learning_id": fmt.Sprintf("%d", id),
			"project":     "",
			"category":    "gotcha",
		},
	}}); err != nil {
		t.Fatalf("add anticipated query vector: %v", err)
	}

	results := hybridSearchRun(t, h, "zzz qqq")

	if _, ok := results[fmt.Sprintf("%d", id)]; ok {
		t.Errorf("quarantined learning %d surfaced through the metadata block", id)
	}
}

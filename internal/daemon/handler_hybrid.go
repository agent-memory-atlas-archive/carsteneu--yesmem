package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// resultCooldown keeps freshly written learnings out of search results: one written
// minutes ago has not proven any retrieval value yet.
const resultCooldown = 30 * time.Minute

// maxSkipRecent bounds the skip_recent parameter. Callers ask for a handful of
// just-extracted rows; the cap only keeps a malformed value from loading the
// whole ID set.
const maxSkipRecent = 1000

// maxHybridSearchLimit caps the caller-supplied hybrid_search limit. It mirrors
// maxVectorSearchLimit: the value bounds slices and a LIMIT clause in both lanes.
const maxHybridSearchLimit = 200

// vectorSearchOversample is how many extra hits vector_search asks the store for
// so the quarantine gate can drop the in-memory anticipated-query hits (the only
// ones it can reach) without returning a short result set.
const vectorSearchOversample = 8

// maxVectorSearchLimit caps the caller-supplied limit. The IDs it produces are
// bound into the quarantine lookup, so an unbounded value would hit SQLite's
// variable ceiling and disable the gate.
const maxVectorSearchLimit = 200

// unfinishedCategoryDampening is the score factor for an unfinished learning.
// One thread is worth noting, but it is not a settled fact, so it competes with
// the settled rows at a discount.
const unfinishedCategoryDampening = 0.7

// sourceBoostFor returns the trust multiplier for a learning source. Search hits get
// it from the metadata stage; graph neighbours need it for their derived score.
func sourceBoostFor(source string) float64 {
	switch source {
	case "user_stated":
		return 1.25
	case "agreed_upon":
		return 1.15
	case "hook_auto_learned":
		return 1.10
	}
	return 1.0
}

// learningGates carries the per-learning attributes a result is judged on, in one
// value so a direct hit and a graph neighbour can be judged by the same function.
// A neighbour that skipped a gate smuggled content past a filter its own parent
// had to pass (YM-462).
type learningGates struct {
	category    string
	source      string
	createdAt   time.Time
	quarantined bool
}

// applyLearningGates applies the gates every result must pass to stay in the
// result set: quarantine and narrative drop, the result cooldown, and for the
// survivors the unfinished dampening and the source boost. relation is the
// association edge that produced the hit, "" for a direct hit. A contradiction is
// surfaced as an explicit warning (proxy), never as supporting context, so a
// contradicts edge is dropped here rather than scored.
func applyLearningGates(r embedding.RankedResult, g learningGates, relation string) (embedding.RankedResult, bool) {
	if g.quarantined || g.category == "narrative" || relation == "contradicts" {
		return r, false
	}
	if !g.createdAt.IsZero() && time.Since(g.createdAt) < resultCooldown {
		return r, false
	}
	if g.category == "unfinished" {
		r.Score *= unfinishedCategoryDampening
	}
	r.Score *= sourceBoostFor(g.source)
	return r, true
}

// embedJob represents a learning to be embedded asynchronously.
type embedJob struct {
	id       int64
	content  string
	category string
	project  string
}

// VectorStore returns the handler's vector store (may be nil if not configured).
func (h *Handler) VectorStore() *embedding.VectorStore {
	h.embedMu.RLock()
	defer h.embedMu.RUnlock()
	return h.embed.vectorStore
}

// SetEmbedding configures the handler with vector search capabilities.
// If not called, hybrid_search falls back to BM25-only.
//
// Each field is set on its own: the dedicated query provider is published before
// this call (daemon.go), so republishing the whole set would clear it again.
func (h *Handler) SetEmbedding(indexer *embedding.Indexer, store *embedding.VectorStore, provider embedding.Provider) {
	h.embedMu.Lock()
	h.embed.indexer = indexer
	h.embed.vectorStore = store
	h.embed.embedProvider = provider
	h.embedMu.Unlock()
}

// SetSearchEmbeddingProvider configures a dedicated provider for query embeddings.
func (h *Handler) SetSearchEmbeddingProvider(provider embedding.Provider) {
	h.embedMu.Lock()
	h.embed.searchProvider = provider
	h.embedMu.Unlock()
}

// SetIVFPath publishes where the IVF index is persisted. SaveIVF reads the path
// on daemon shutdown, long after the startup goroutine that publishes it.
func (h *Handler) SetIVFPath(path string) {
	h.embedMu.Lock()
	h.embed.ivfPath = path
	h.embedMu.Unlock()
}

// embedSnapshot returns a copy of the currently published wiring, taken under
// the lock. A reader therefore sees each field either before or after a publish,
// never a torn value; the setters publish field by field, so a snapshot between
// two of them is a valid mix (for example a vector store without an IVF path
// yet) and every caller must handle each field's nil/empty case on its own.
func (h *Handler) embedSnapshot() embedWiring {
	h.embedMu.RLock()
	defer h.embedMu.RUnlock()
	return h.embed
}

// SaveIVF persists the IVF index to disk if configured.
func (h *Handler) SaveIVF() {
	w := h.embedSnapshot()
	if w.vectorStore == nil || w.ivfPath == "" {
		return
	}
	if err := w.vectorStore.SaveIVF(w.ivfPath); err != nil {
		log.Printf("SaveIVF: %v", err)
	}
}

// SetEmbedProcess stores the embed-learnings subprocess for lifecycle management.
func (h *Handler) SetEmbedProcess(p *os.Process) {
	h.embedProcessMu.Lock()
	h.embedProcess = p
	h.embedProcessMu.Unlock()
}

// StopEmbedProcess kills the embed-learnings subprocess if running.
// Called during daemon shutdown. Per-batch commit ensures no progress is lost.
func (h *Handler) StopEmbedProcess() {
	h.embedProcessMu.Lock()
	p := h.embedProcess
	h.embedProcess = nil
	h.embedProcessMu.Unlock()
	if p != nil {
		log.Printf("Stopping embed-learnings subprocess (PID %d)", p.Pid)
		p.Kill()
	}
}

// EmbedProvider returns the handler's embedding provider (may be nil).
func (h *Handler) EmbedProvider() embedding.Provider {
	h.embedMu.RLock()
	defer h.embedMu.RUnlock()
	return h.embed.embedProvider
}

// queryProvider returns the provider for query embeddings, preferring the
// dedicated one published through SetSearchEmbeddingProvider.
func (w embedWiring) queryProvider() embedding.Provider {
	if w.searchProvider != nil {
		return w.searchProvider
	}
	return w.embedProvider
}

// StartEmbedWorker starts a background goroutine that processes embed jobs.
// Call this after SetEmbedding. The worker stops when ctx is cancelled.
func (h *Handler) StartEmbedWorker(ctx context.Context) {
	h.embedMu.Lock()
	if h.embed.indexer == nil {
		h.embedMu.Unlock()
		return
	}
	queue := make(chan embedJob, 100)
	h.embed.queue = queue
	h.embedMu.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				// Drain remaining jobs before exit
				for {
					select {
					case job := <-queue:
						h.processEmbedJob(context.Background(), job)
					default:
						return
					}
				}
			case job := <-queue:
				h.processEmbedJob(ctx, job)
			}
		}
	}()
}

func (h *Handler) processEmbedJob(ctx context.Context, job embedJob) {
	w := h.embedSnapshot()
	err := w.indexer.Index(ctx, fmt.Sprintf("%d", job.id), job.content, map[string]string{
		"category": job.category,
		"project":  job.project,
	})
	if err != nil {
		log.Printf("auto-embed learning #%d: %v", job.id, err)
		return
	}
	if h.store != nil {
		if err := h.store.MarkEmbeddingsDone([]int64{job.id}); err != nil {
			log.Printf("auto-embed mark done #%d: %v", job.id, err)
		}
	}
	runtime.Gosched() // yield after inference so other goroutines can run
}

// RemoveEmbedding removes a learning's embedding from the vector store.
// Safe to call even when embedding is not configured (no-op).
func (h *Handler) RemoveEmbedding(id int64) {
	vecStore := h.VectorStore()
	if vecStore == nil {
		return
	}
	ctx := context.Background()
	if err := vecStore.Delete(ctx, fmt.Sprintf("%d", id)); err != nil {
		log.Printf("remove embedding #%d: %v", id, err)
	}
}

// EmbedLearning queues a learning for async embedding.
// Safe to call even when embedding is not configured (no-op).
func (h *Handler) EmbedLearning(id int64, content, category, project string) {
	h.embedMu.RLock()
	queue := h.embed.queue
	h.embedMu.RUnlock()
	if queue == nil {
		return
	}
	select {
	case queue <- embedJob{id: id, content: content, category: category, project: project}:
	default:
		log.Printf("auto-embed: queue full, dropping learning #%d", id)
	}
}

// recentLearningIDs returns the IDs of the n most recently created learnings, or
// nil when n asks for nothing. With a session id the window covers only that
// session's own rows, so inserts from any other session cannot push the caller's
// just-extracted learning out of it. A thread that is not a session id (a
// content-hash thread) has no rows of its own; there the global window still
// applies.
func (h *Handler) recentLearningIDs(n int, sessionID string) map[string]bool {
	if n <= 0 || h.store == nil {
		return nil
	}
	if n > maxSkipRecent {
		n = maxSkipRecent
	}
	ids, err := h.store.GetRecentLearningIDs(n, sessionID)
	if err != nil {
		log.Printf("hybrid_search: skip_recent lookup failed: %v", err)
		return nil
	}
	if len(ids) == 0 && sessionID != "" {
		ids, err = h.store.GetRecentLearningIDs(n, "")
		if err != nil {
			log.Printf("hybrid_search: skip_recent fallback lookup failed: %v", err)
			return nil
		}
	}
	if len(ids) == 0 {
		return nil
	}
	skip := make(map[string]bool, len(ids))
	for _, id := range ids {
		skip[strconv.FormatInt(id, 10)] = true
	}
	return skip
}

// dropSkippedResults removes results whose learning ID is in skip. It returns a
// fresh slice: callers keep their lane slices, so an in-place rewrite of the
// caller's backing array would be a trap for the next reader.
func dropSkippedResults(results []embedding.RankedResult, skip map[string]bool) []embedding.RankedResult {
	if len(skip) == 0 || len(results) == 0 {
		return results
	}
	out := make([]embedding.RankedResult, 0, len(results))
	for _, r := range results {
		if !skip[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

func (h *Handler) handleHybridSearch(params map[string]any) Response {
	t0 := time.Now()
	query, _ := params["query"].(string)
	project, _ := params["project"].(string)
	since, _ := params["since"].(string)
	before, _ := params["before"].(string)
	limit := intOr(params, "limit", 10)
	// The limit reaches two slices and one LIMIT clause, plus the AQ lane's
	// limit*3. A negative value panics on the slice, an absurd value wraps the
	// multiply negative and SQLite then reads it as "no limit".
	if limit < 0 {
		limit = 0
	}
	if limit > maxHybridSearchLimit {
		limit = maxHybridSearchLimit
	}

	if query == "" {
		return errorResponse("query is required")
	}
	if since != "" && storage.ParseFilterTime(since).IsZero() {
		return errorResponse(fmt.Sprintf("since is not a parseable timestamp: %q", truncate(since, 80)))
	}
	if before != "" && storage.ParseFilterTime(before).IsZero() {
		return errorResponse(fmt.Sprintf("before is not a parseable timestamp: %q", truncate(before, 80)))
	}

	if limit == 0 {
		// Same meaning as in get_learnings: nothing requested. Without this the
		// bm25_only branch and RRFMerge skip truncation for a non-positive limit and
		// return the whole lane, whose size comes from the corpus.
		return jsonResponse(map[string]any{
			"results":       []any{},
			"bm25_count":    0,
			"vector_count":  0,
			"merged_count":  0,
			"fusion_method": "none",
			"fusion_k":      60,
			"message":       "Hybrid search: limit is 0, no results requested",
		})
	}

	// skip_recent keeps the newest N learnings out of the result set. The proxy's
	// associative context uses it so a learning cannot be injected into the turn
	// that just extracted it (echo injection). The skip window follows the
	// caller's own session when it names one: a global window over the newest IDs
	// is one insert from any other session away from dropping the target.
	skipSession, _ := params["session_id"].(string)
	skipIDs := h.recentLearningIDs(intOr(params, "skip_recent", 0), skipSession)

	const searchTimeout = 5 * time.Second

	// Dynamic lane limit: scale with corpus size so decay-reranking has enough candidates
	laneLimit := limit
	if h.store != nil {
		var corpusSize int
		h.store.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE superseded_by IS NULL`).Scan(&corpusSize)
		if dl := corpusSize / 10; dl > laneLimit {
			laneLimit = dl
		}
		if laneLimit > 1000 {
			laneLimit = 1000
		}
	}

	// Run BM25 and Vector search in parallel with individual timeouts
	type bm25Result struct {
		results []embedding.RankedResult
		err     error
	}
	type vectorResult struct {
		results  []embedding.RankedResult
		queryVec []float32
		err      error
	}

	bm25Ch := make(chan bm25Result, 1)
	vectorCh := make(chan vectorResult, 1)
	aqCh := make(chan bm25Result, 1)

	// BM25 goroutine
	go func() {
		if h.store == nil {
			bm25Ch <- bm25Result{}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
		defer cancel()
		results, err := h.store.SearchLearningsBM25Ctx(ctx, query, project, since, before, laneLimit)
		if err != nil {
			bm25Ch <- bm25Result{err: err}
			return
		}
		var ranked []embedding.RankedResult
		for _, r := range results {
			ranked = append(ranked, embedding.RankedResult{
				ID: r.ID, Content: r.Content, Score: r.Score,
				OriginalScore: r.Score, // negated BM25 score (higher=better)
				Project: r.Project,
			})
		}
		bm25Ch <- bm25Result{results: ranked}
	}()

	// Vector goroutine
	go func() {
		w := h.embedSnapshot()
		queryProvider := w.queryProvider()
		if queryProvider == nil || !queryProvider.Enabled() || w.vectorStore == nil {
			vectorCh <- vectorResult{}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
		defer cancel()
		vectors, err := queryProvider.Embed(ctx, []string{query})
		if err != nil {
			vectorCh <- vectorResult{err: err}
			return
		}
		if len(vectors) == 0 {
			vectorCh <- vectorResult{}
			return
		}

		var searchResults []embedding.SearchResult
		if since != "" || before != "" {
			// Date prefilter: SQLite ID lookup → brute-force scan over allowed IDs
			allowedIDs, idErr := h.getDateFilteredIDs(since, before, project)
			if idErr != nil {
				vectorCh <- vectorResult{err: idErr}
				return
			}
			searchResults, err = w.vectorStore.SearchAllowed(ctx, vectors[0], laneLimit, allowedIDs)
		} else {
			searchResults, err = w.vectorStore.SearchWithProject(ctx, vectors[0], laneLimit, project)
		}
		if err != nil {
			vectorCh <- vectorResult{err: err}
			return
		}
		var ranked []embedding.RankedResult
		for _, r := range searchResults {
			ranked = append(ranked, embedding.RankedResult{
				ID: r.ID, Content: r.Content,
				Score:         float64(r.Similarity),
				OriginalScore: float64(r.Similarity),
				Project:       r.Metadata["project"],
			})
		}
		vectorCh <- vectorResult{results: ranked, queryVec: vectors[0]}
	}()

	// AQ-FTS goroutine (porter stemming — catches synonyms like dog/puppy)
	go func() {
		if h.store == nil {
			aqCh <- bm25Result{}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
		defer cancel()
		_ = ctx // ctx timeout handled via store's busy_timeout
		results, err := h.store.SearchAnticipatedQueries(query, project, limit, since, before)
		if err != nil {
			aqCh <- bm25Result{err: err}
			return
		}
		var ranked []embedding.RankedResult
		for _, r := range results {
			ranked = append(ranked, embedding.RankedResult{
				ID: r.ID, Content: r.Content, Score: r.Score,
				OriginalScore: r.Score, Project: r.Project,
			})
		}
		aqCh <- bm25Result{results: ranked}
	}()

	// Collect results
	bm25Res := <-bm25Ch
	vectorRes := <-vectorCh
	aqRes := <-aqCh

	bm25Results := bm25Res.results
	vectorResults := vectorRes.results
	aqResults := aqRes.results

	// Drop skipped learnings before fusion. The lane fetch is already capped at
	// laneLimit, so this cannot pull replacements in; it keeps the skipped rows
	// out of the RRF ranks and out of the merged set.
	bm25Results = dropSkippedResults(bm25Results, skipIDs)
	vectorResults = dropSkippedResults(vectorResults, skipIDs)
	aqResults = dropSkippedResults(aqResults, skipIDs)

	// Merge AQ-FTS results into BM25 (dedup by ID, boost existing entries)
	if len(aqResults) > 0 {
		bm25IDs := make(map[string]int, len(bm25Results))
		for i, r := range bm25Results {
			bm25IDs[r.ID] = i
		}
		for _, aq := range aqResults {
			if _, exists := bm25IDs[aq.ID]; !exists {
				bm25Results = append(bm25Results, aq)
			}
		}
	}

	if bm25Res.err != nil {
		log.Printf("hybrid_search: bm25 failed in %v: %v", time.Since(t0), bm25Res.err)
	}
	if vectorRes.err != nil {
		log.Printf("hybrid_search: vector failed in %v: %v", time.Since(t0), vectorRes.err)
	}
	if aqRes.err != nil {
		log.Printf("hybrid_search: aq-fts failed: %v", aqRes.err)
	}
	tSearch := time.Since(t0)

	// If both paths failed, return error
	if len(bm25Results) == 0 && len(vectorResults) == 0 && (bm25Res.err != nil || vectorRes.err != nil) {
		return errorResponse(fmt.Sprintf("search failed: bm25=%v, vector=%v", bm25Res.err, vectorRes.err))
	}

	// Determine fusion method
	fusionMethod := "rrf"
	if len(vectorResults) == 0 {
		fusionMethod = "bm25_only"
	}

	// Merge via RRF or normalize BM25-only scores
	if fusionMethod == "bm25_only" {
		for i := range bm25Results {
			bm25Results[i].Source = "keyword"
		}
		// Scores already pre-normalized by tier in SearchLearningsBM25Ctx (0-100).
		// Just assign source and cap to limit.
		if len(bm25Results) > limit {
			bm25Results = bm25Results[:limit]
		}
	}

	var merged []embedding.RankedResult
	if fusionMethod == "rrf" {
		merged = embedding.RRFMerge(bm25Results, vectorResults, 60, limit)
	} else {
		merged = bm25Results
	}

	// Resolve superseded learnings: redirect to active successor
	tMerge := time.Since(t0)
	merged = h.resolveSupersededResults(merged)
	tSupersede := time.Since(t0)

	// Bulk metadata lookup: category, source, created_at, quarantine in a single
	// round-trip. Replaces the old GetCategoriesBulk + separate source + separate
	// cooldown queries.
	if len(merged) > 0 {
		ids := make([]string, len(merged))
		for i, r := range merged {
			ids[i] = r.ID
		}
		placeholders := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args[i] = id
		}
		type bulkMeta struct {
			category    string
			source      string
			createdAt   string
			quarantined bool
		}
		bulk := make(map[string]bulkMeta, len(ids))
		rows, err := h.store.ReaderDB().Query("SELECT CAST(id AS TEXT), COALESCE(category, ''), COALESCE(source, ''), created_at, COALESCE(quarantined_at, '') FROM learnings WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
		if err != nil {
			// Fail-open by construction: without rows there is no metadata, so the
			// quarantine branch below cannot fire. Make the degradation visible.
			log.Printf("hybrid_search: metadata lookup failed, quarantine filter degraded: %v", err)
		} else {
			defer rows.Close()
			for rows.Next() {
				var id, cat, src, created, quarantinedAt string
				if rows.Scan(&id, &cat, &src, &created, &quarantinedAt) == nil {
					bulk[id] = bulkMeta{category: cat, source: src, createdAt: created, quarantined: quarantinedAt != ""}
				}
			}
		}

		// Every result passes the per-learning gates, whichever lane produced it.
		// A hit without a metadata row cannot be judged and passes unchanged, as it
		// always has (fail-open: no metadata, no gate).
		{
			filtered := merged[:0]
			for _, r := range merged {
				bm, ok := bulk[r.ID]
				if !ok {
					filtered = append(filtered, r)
					continue
				}
				gated, keep := applyLearningGates(r, learningGates{
					category:    bm.category,
					source:      bm.source,
					createdAt:   storage.ParseTimestamp(bm.createdAt),
					quarantined: bm.quarantined,
				}, "")
				if keep {
					filtered = append(filtered, gated)
				}
			}
			merged = filtered
		}
	}

	tCategory := time.Since(t0)

	// Apply cluster-affinity boost: learnings that perform well in the current query cluster rank higher
	// Cluster-affinity needs a query vector to find the nearest cluster.
	if h.store != nil && len(merged) > 0 && len(vectorRes.queryVec) > 0 {
		clusters, err := h.store.GetQueryClusters("")
		if err != nil {
			log.Printf("hybrid_search: cluster load failed: %v", err)
		}
		if len(clusters) > 0 {
			nearestCluster := storage.FindNearestCluster(vectorRes.queryVec, clusters, 0.75)
			if nearestCluster > 0 {
				var lids []string
				for _, r := range merged {
					lids = append(lids, r.ID)
				}
				clusterScores, err := h.store.GetClusterScoresForLearnings(lids)
				if err != nil {
					log.Printf("hybrid_search: cluster scores failed: %v", err)
				}
				affinityApplied := false
				for i, r := range merged {
					scores, ok := clusterScores[r.ID]
					if !ok {
						continue
					}
					for _, cs := range scores {
						if cs.ClusterID != nearestCluster || cs.InjectCount < 3 {
							continue
						}
						useRate := float64(cs.UseCount) / float64(cs.InjectCount)
						noiseRate := float64(cs.NoiseCount) / float64(cs.InjectCount)
						affinity := 1.0 + useRate - noiseRate*0.5
						if affinity < 0.3 {
							affinity = 0.3
						}
						if affinity > 1.8 {
							affinity = 1.8
						}
						merged[i].Score *= affinity
						affinityApplied = true
					}
				}
				if affinityApplied {
					sort.Slice(merged, func(i, j int) bool {
						return merged[i].Score > merged[j].Score
					})
				}
			}
		}
	}

	// Hub dampening: learnings appearing in many clusters are generic — penalize regardless of vector availability.
	// Runs independently of cluster-affinity (which needs a query vector).
	if h.store != nil && len(merged) > 0 {
		var hubIDs []string
		for _, r := range merged {
			hubIDs = append(hubIDs, r.ID)
		}
		if hubScores, err := h.store.GetClusterScoresForLearnings(hubIDs); err == nil {
			for i, r := range merged {
				spread := len(hubScores[r.ID])
				if spread > 3 {
					hubPenalty := 1.0 - 0.1*float64(spread-3)
					if hubPenalty < 0.5 {
						hubPenalty = 0.5
					}
					merged[i].Score *= hubPenalty
				}
			}
		}
	}

	// Apply project-recency boost: fresh same-project results rank higher
	merged = h.applyTurnBasedDecay(merged, project)

	// Graph augmentation: expand results with association neighbors. Runs after the
	// metadata stage on purpose — only hits that survived the filters may expand,
	// and neighbours pass the same category and cooldown gates inside.
	merged = h.augmentWithGraphNeighbors(merged)

	// Graph neighbours are added after the lane filters, so the skip gate has to
	// run once more to keep a freshly written neighbour out as well.
	merged = dropSkippedResults(merged, skipIDs)

	// Enrich with subagent metadata
	enrichedResults := h.enrichRankedResults(merged)
	tEnrich := time.Since(t0)

	log.Printf("hybrid_search: done in %v (bm25=%d, vector=%d) [search=%v merge=%v supersede=%v category=%v enrich=%v]",
		tEnrich, len(bm25Results), len(vectorResults),
		tSearch, tMerge-tSearch, tSupersede-tMerge, tCategory-tSupersede, tEnrich-tCategory)

	// Stufe 1: Persist query vector + injected IDs for cluster-scoring (async, non-blocking)
	if h.store != nil && len(vectorRes.queryVec) > 0 {
		var injectedIDs []string
		for _, r := range merged {
			injectedIDs = append(injectedIDs, r.ID)
		}
		qv := vectorRes.queryVec
		go h.store.InsertQueryLog(project, query, qv, injectedIDs)
	}

	return jsonResponse(map[string]any{
		"results":       enrichedResults,
		"bm25_count":    len(bm25Results),
		"vector_count":  len(vectorResults),
		"merged_count":  len(merged),
		"fusion_method": fusionMethod,
		"fusion_k":      60,
		"message":       fmt.Sprintf("Hybrid search: %d BM25 + %d vector → %d merged results", len(bm25Results), len(vectorResults), len(merged)),
		"hint":          fmt.Sprintf("Search for concrete terms from the source (filenames, functions, error messages), not your own summaries. On 0 hits: try different terms.\nIf these results don't answer your question, call deep_search('%s') for raw conversation history (full untruncated content).\nFor structured metadata (files, commands, tags): query_facts(entity=..., action=..., keyword=...).", query),
	})
}

// handleVectorSearch performs vector-only search with cosine similarity scores.
// Returns results with similarity (0-1) for threshold-based filtering.
func (h *Handler) handleVectorSearch(params map[string]any) Response {
	query, _ := params["query"].(string)
	if query == "" {
		return errorResponse("query is required")
	}

	limit := 3
	if l, ok := params["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	// The limit is caller-controlled and becomes an ID list in the quarantine
	// gate, so it needs a ceiling: an unbounded value overflows SQLite's variable
	// cap and would degrade that gate.
	if limit > maxVectorSearchLimit {
		limit = maxVectorSearchLimit
	}

	w := h.embedSnapshot()
	queryProvider := w.queryProvider()
	if queryProvider == nil || !queryProvider.Enabled() || w.vectorStore == nil {
		return errorResponse("vector search not enabled")
	}

	ctx := context.Background()
	vectors, err := queryProvider.Embed(ctx, []string{query})
	if err != nil {
		log.Printf("vector_search: embed failed: %v", err)
		return errorResponse("embedding failed")
	}
	if len(vectors) == 0 {
		return errorResponse("no embedding produced")
	}

	searchResults, err := w.vectorStore.Search(ctx, vectors[0], limit+vectorSearchOversample)
	if err != nil {
		log.Printf("vector_search: search failed: %v", err)
		return errorResponse("vector search failed")
	}

	type vectorResult struct {
		ID              string            `json:"id"`
		Content         string            `json:"content"`
		Similarity      float64           `json:"similarity"`
		Metadata        map[string]string `json:"metadata,omitempty"`
		AgentType       string            `json:"agent_type,omitempty"`
		ParentSessionID string            `json:"parent_session_id,omitempty"`
	}

	// Bulk-load subagent metadata (deduplicate IDs)
	seen := make(map[string]bool, len(searchResults))
	var ids []string
	for _, r := range searchResults {
		if r.ID != "" && !seen[r.ID] {
			seen[r.ID] = true
			ids = append(ids, r.ID)
		}
	}
	meta, _ := h.store.GetSessionMetaBulk(ids)
	// Quarantine gate: the vector store filters quarantined rows on its SQL
	// paths, but anticipated-query vectors live in memory and resolve to their
	// parent through metadata only. Ask for the quarantined subset once instead
	// of trusting the lane that produced the hit. Fail closed: a lookup that
	// cannot run must not return quarantined content.
	quarantined, err := h.store.QuarantinedIDs(ids)
	if err != nil {
		log.Printf("vector_search: quarantine lookup failed: %v", err)
		return errorResponse("quarantine check failed")
	}

	results := make([]vectorResult, 0, len(searchResults))
	for _, r := range searchResults {
		if len(results) >= limit {
			break
		}
		if quarantined[r.ID] {
			continue
		}
		vr := vectorResult{
			ID:         r.ID,
			Content:    r.Content,
			Similarity: float64(r.Similarity),
			Metadata:   r.Metadata,
		}
		if m, ok := meta[r.ID]; ok {
			vr.AgentType = m.AgentType
			vr.ParentSessionID = m.ParentSessionID
		}
		results = append(results, vr)
	}

	return jsonResponse(results)
}

// resolveSupersededResults replaces superseded learning hits with their active
// successors. Follows supersede chains (A→B→C) and deduplicates results.
// Learnings with negative superseded_by (bulk-resolved, no valid successor) are dropped.
func (h *Handler) resolveSupersededResults(results []embedding.RankedResult) []embedding.RankedResult {
	if h.store == nil || len(results) == 0 {
		return results
	}

	// Collect numeric IDs
	var ids []int64
	for _, r := range results {
		if id, err := strconv.ParseInt(r.ID, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}

	// Phase 1: Redirect superseded learnings to active successors
	redirects, _ := h.store.ResolveSupersededIDs(ids)
	seen := map[string]bool{}
	out := make([]embedding.RankedResult, 0, len(results))
	for _, r := range results {
		id, _ := strconv.ParseInt(r.ID, 10, 64)
		if target, ok := redirects[id]; ok {
			if target <= 0 {
				continue // chain end has no active learning — drop the hit
			}
			active, err := h.store.GetLearning(target)
			if err != nil {
				continue
			}
			activeID := strconv.FormatInt(active.ID, 10)
			if seen[activeID] {
				continue
			}
			seen[activeID] = true
			out = append(out, embedding.RankedResult{
				ID:      activeID,
				Content: active.Content,
				Score:   r.Score * 0.7, // penalty: found via old content
				Source:  r.Source,
				Project: active.Project,
			})
		} else {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
	}

	// Phase 2: Staleness penalty — reduce score for learnings with high staleness_score
	if len(out) > 0 {
		var outIDs []int64
		for _, r := range out {
			if id, err := strconv.ParseInt(r.ID, 10, 64); err == nil {
				outIDs = append(outIDs, id)
			}
		}
		scores, err := h.store.GetStalenessScores(outIDs)
		if err == nil && len(scores) > 0 {
			out = applyStalenessPenalty(out, scores)
		}
	}

	return out
}

// applyStalenessPenalty applies staleness score penalty to results.
// staleness_score >= 0.8 → 0.5×, staleness_score >= 0.5 → 0.8×
func applyStalenessPenalty(results []embedding.RankedResult, scores map[int64]float64) []embedding.RankedResult {
	if len(scores) == 0 {
		return results
	}
	for i, r := range results {
		if id, err := strconv.ParseInt(r.ID, 10, 64); err == nil {
			if ss, ok := scores[id]; ok {
				if ss >= 0.8 {
					results[i].Score *= 0.5
				} else if ss >= 0.5 {
					results[i].Score *= 0.8
				}
			}
		}
	}
	return results
}

// augmentWithGraphNeighbors expands search results with graph-connected learnings.
// Looks up all outgoing association edges for result IDs, loads missing neighbors,
// and inserts them with a derived score. Max 3 per source, 5 total.
func (h *Handler) augmentWithGraphNeighbors(merged []embedding.RankedResult) []embedding.RankedResult {
	if h.store == nil || len(merged) == 0 {
		return merged
	}

	const maxPerSource = 3
	const maxTotal = 5

	var ids []string
	existingIDs := make(map[string]bool)
	for _, r := range merged {
		ids = append(ids, r.ID)
		existingIDs[r.ID] = true
	}

	neighbors, err := h.store.GetAssociationNeighbors(ids, maxPerSource)
	if err != nil {
		log.Printf("graph augment: neighbor lookup failed: %v", err)
		return merged
	}

	added := 0
	for _, r := range merged {
		if added >= maxTotal {
			break
		}
		edges, ok := neighbors[r.ID]
		if !ok {
			continue
		}
		for _, edge := range edges {
			if added >= maxTotal {
				break
			}
			if existingIDs[edge.TargetID] {
				continue
			}

			targetID, err := strconv.ParseInt(edge.TargetID, 10, 64)
			if err != nil {
				continue
			}
			// GetAssociationNeighbors already filters superseded+quarantined via JOIN
			learning, err := h.store.GetLearning(targetID)
			if err != nil || learning == nil {
				continue
			}

			// Derive score: parent * base multiplier, modulated by weight (capped at 3.0).
			// depends_on/supports edges get higher multipliers — prerequisites and
			// supporting evidence are more valuable than generic associations.
			weight := edge.Weight
			if weight > 3.0 {
				weight = 3.0
			}
			var baseMultiplier float64
			var source string
			switch edge.RelationType {
			case "depends_on":
				baseMultiplier = 0.8
				source = "graph:depends_on"
			case "supports":
				baseMultiplier = 0.7
				source = "graph:supports"
			default:
				baseMultiplier = 0.6
				source = "graph"
			}

			// The gates run after the derived score and through the same function the
			// direct hits use, so a neighbour and a direct hit of the same learning
			// come out with the same adjustments: unfinished dampening, cooldown,
			// source boost, and a contradicts edge dropped instead of scored.
			gated, keep := applyLearningGates(embedding.RankedResult{
				ID:      edge.TargetID,
				Content: learning.Content,
				Score:   r.Score * baseMultiplier * weight / 3.0,
				Source:  source,
				Project: learning.Project,
			}, learningGates{
				category:  learning.Category,
				source:    learning.Source,
				createdAt: learning.CreatedAt,
			}, edge.RelationType)
			if !keep {
				continue
			}

			merged = append(merged, gated)
			existingIDs[edge.TargetID] = true
			added++
			log.Printf("graph augment: added [ID:%s] via %s edge from [ID:%s] (score=%.2f, weight=%.1f)",
				edge.TargetID, edge.RelationType, r.ID, gated.Score, edge.Weight)
		}
	}

	if added > 0 {
		sort.Slice(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	}
	return merged
}

// applyTurnBasedDecay re-ranks merged results by applying turn-based decay
// to the RRF score. Learnings with many project turns since creation decay,
// learnings in paused projects stay at full relevance.
func (h *Handler) applyTurnBasedDecay(results []embedding.RankedResult, project string) []embedding.RankedResult {
	if h.store == nil || len(results) == 0 {
		return results
	}

	// Bulk lookup decay-relevant fields for all result IDs
	placeholders := make([]string, len(results))
	args := make([]any, len(results))
	for i, r := range results {
		placeholders[i] = "?"
		args[i] = r.ID
	}
	rows, err := h.store.DB().Query("SELECT CAST(id AS TEXT), COALESCE(project, ''), COALESCE(source, ''), COALESCE(use_count, 0), COALESCE(save_count, 0), COALESCE(stability, 30.0), COALESCE(turns_at_creation, 0) FROM learnings WHERE id IN ("+strings.Join(placeholders, ",")+") AND (valid_until IS NULL OR julianday(valid_until) > julianday('now'))", args...)
	if err != nil {
		log.Printf("applyTurnBasedDecay: query failed: %v", err)
		return results
	}
	defer rows.Close()

	type decayMeta struct {
		project         string
		source          string
		useCount        int
		saveCount       int
		stability       float64
		turnsAtCreation int64
	}
	metaMap := make(map[string]decayMeta, len(results))
	projectSet := make(map[string]bool)
	hasGlobal := false
	for rows.Next() {
		var id, proj, source string
		var useCount, saveCount int
		var stability float64
		var turnsAtCreation int64
		if rows.Scan(&id, &proj, &source, &useCount, &saveCount, &stability, &turnsAtCreation) == nil {
			metaMap[id] = decayMeta{project: proj, source: source, useCount: useCount, saveCount: saveCount, stability: stability, turnsAtCreation: turnsAtCreation}
			if proj != "" {
				projectSet[proj] = true
			} else {
				hasGlobal = true
			}
		}
	}

	// Bulk-fetch current turn counts for all projects in results
	projectList := make([]string, 0, len(projectSet)+1)
	for p := range projectSet {
		projectList = append(projectList, p)
	}
	if hasGlobal {
		projectList = append(projectList, "__global__")
	}
	turnCounts, _ := h.store.GetTurnCountsBulk(projectList)

	// Apply turn-based decay as multiplicative factor
	for i, r := range results {
		m, ok := metaMap[r.ID]
		if !ok {
			continue
		}
		turnKey := m.project
		if turnKey == "" {
			turnKey = "__global__"
		}
		currentTurn := turnCounts[turnKey]
		decay := models.TurnBasedDecay(m.turnsAtCreation, currentTurn, m.stability, m.source, m.useCount, m.saveCount)
		results[i].Score *= decay
	}

	// Re-sort by decayed score
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}

// getDateFilteredIDs returns the set of learning IDs that pass the date and project filters.
// Used as pre-filter for the vector lane to restrict brute-force scan to a small ID set.
func (h *Handler) getDateFilteredIDs(since, before, project string) (map[string]bool, error) {
	where := `WHERE ` + models.RecallableLearningSQL + ` AND embedding_vector IS NOT NULL`
	args := make([]any, 0, 3)
	// julianday() on the column is what makes the mixed offsets in created_at
	// comparable; the bound is normalized so its spelling never matters.
	if since != "" {
		where += ` AND julianday(created_at) >= julianday(?)`
		args = append(args, storage.SQLiteFilterTime(since))
	}
	if before != "" {
		where += ` AND julianday(created_at) < julianday(?)`
		args = append(args, storage.SQLiteFilterTime(before))
	}
	if project != "" {
		where += ` AND project = ?`
		args = append(args, project)
	}

	rows, err := h.store.DB().Query(`SELECT CAST(id AS TEXT) FROM learnings `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("getDateFilteredIDs: %w", err)
	}
	defer rows.Close()

	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids[id] = true
		}
	}
	return ids, rows.Err()
}

// enrichRankedResults adds subagent metadata and agent_role to RankedResult slices (used by hybrid_search).
func (h *Handler) enrichRankedResults(results []embedding.RankedResult) []map[string]any {
	seen := make(map[string]bool, len(results))
	var ids []string
	for _, r := range results {
		if r.ID != "" && !seen[r.ID] {
			seen[r.ID] = true
			ids = append(ids, r.ID)
		}
	}
	roles, _ := h.store.GetAgentRolesBulk(ids)

	// Bulk-fetch learning source + category fields plus the owning session metadata.
	sources := map[string]string{}
	categories := map[string]string{}
	agentTypes := map[string]string{}
	parentSessionIDs := map[string]string{}
	sourceAgents := map[string]string{}
	originTools := map[string]string{}
	if h.store != nil && len(ids) > 0 {
		placeholders := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := h.store.ReaderDB().Query(`
			SELECT CAST(l.id AS TEXT),
				COALESCE(l.source, ''),
				COALESCE(l.category, ''),
				COALESCE(s.agent_type, ''),
				COALESCE(s.parent_session_id, ''),
				COALESCE(s.source_agent, 'claude'),
				COALESCE(l.origin_tool, '')
			FROM learnings l
			LEFT JOIN sessions s ON s.id = l.session_id
			WHERE l.id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id, src, cat, agentType, parentSessionID, sourceAgent, originTool string
				if rows.Scan(&id, &src, &cat, &agentType, &parentSessionID, &sourceAgent, &originTool) == nil {
					sources[id] = src
					categories[id] = cat
					agentTypes[id] = agentType
					parentSessionIDs[id] = parentSessionID
					sourceAgents[id] = sourceAgent
					originTools[id] = originTool
				}
			}
		}
	}

	out := make([]map[string]any, len(results))
	for i, r := range results {
		m := map[string]any{
			"id":      r.ID,
			"content": r.Content,
			"score":   r.Score,
			"source":  r.Source,
			"project": r.Project,
		}
		if src, ok := sources[r.ID]; ok {
			m["learning_source"] = src
		}
		if cat, ok := categories[r.ID]; ok && cat != "" {
			m["category"] = cat
		}
		if sourceAgent, ok := sourceAgents[r.ID]; ok && sourceAgent != "" {
			m["source_agent"] = sourceAgent
		}
		if agentType, ok := agentTypes[r.ID]; ok && agentType != "" {
			m["agent_type"] = agentType
		}
		if parentSessionID, ok := parentSessionIDs[r.ID]; ok && parentSessionID != "" {
			m["parent_session_id"] = parentSessionID
		}
		if role, ok := roles[r.ID]; ok && role != "" {
			m["agent_role"] = role
		}
		if origTool, ok := originTools[r.ID]; ok && origTool != "" {
			m["origin_tool"] = origTool
		}
		out[i] = m
	}
	return out
}

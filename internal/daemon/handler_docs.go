package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/ingest"
	"github.com/carsteneu/yesmem/internal/repo"
	"github.com/carsteneu/yesmem/internal/storage"
)

var embedDocChunksMu sync.Mutex

const (
	// maxDocsSearchQueryRunes bounds the query. Every call pays one BM25 pass
	// and one embedding request, so an unbounded query is unbounded cost.
	maxDocsSearchQueryRunes = 2000

	docsSearchDefaultLimit = 5
	// docsSearchMaxLimit caps the caller supplied limit. It sits far above any
	// real use on purpose: its job is to stop an absurd value, not to police
	// callers. The clamp is mandatory because limit reaches a slice bound, an
	// oversampling multiply and two SQL LIMITs.
	docsSearchMaxLimit = 1000

	// contextualDocsDefaultLimit is what the caller gets when it names no limit.
	contextualDocsDefaultLimit = 3
	// contextualDocsMaxLimit bounds the response. These chunks do not go to a
	// caller that asked for a list, they go into a prompt, so the bound sits
	// tighter than docsSearchMaxLimit.
	contextualDocsMaxLimit = 100
)

// handleDocsSearch handles the "docs_search" daemon command.
// Parameters: query (required), source (optional), section (optional), project (optional),
// exact (optional bool), limit (optional int), extensions (optional []string), doc_type (optional string)
func (h *Handler) handleDocsSearch(params map[string]any) Response {
	query, _ := params["query"].(string)
	if query == "" {
		return errorResponse("query is required")
	}
	if n := utf8.RuneCountInString(query); n > maxDocsSearchQueryRunes {
		return errorResponse(fmt.Sprintf("query is too long: %d runes, maximum is %d", n, maxDocsSearchQueryRunes))
	}
	source := stringOr(params, "source", "")
	section := stringOr(params, "section", "")
	// Canonicalize the caller project (git worktree -> main repo) so the
	// project filtering and boost below match repo-scoped sources.
	project := repo.RootOrSelf(stringOr(params, "project", ""))
	since, _ := params["since"].(string)
	before, _ := params["before"].(string)
	limit := docsSearchDefaultLimit
	// Read the float directly instead of going through intOr: above MaxInt64 the
	// conversion wraps to a negative value, which no later clamp can tell apart
	// from a caller's negative limit. limit=0 keeps meaning "no results".
	if v, ok := params["limit"].(float64); ok {
		// NaN fails both comparisons and int(NaN) is a negative slice bound.
		if v < 0 || math.IsNaN(v) {
			return errorResponse(fmt.Sprintf("limit must not be negative (got %v)", v))
		}
		if v > docsSearchMaxLimit {
			limit = docsSearchMaxLimit
		} else {
			limit = int(v)
		}
	}
	exact, _ := params["exact"].(bool)
	docType := stringOr(params, "doc_type", "")

	// Resolve optional extensions filter to source IDs.
	// When extensions provided: only search sources whose trigger_extensions match.
	// When only doc_type provided: filter all sources by type.
	var sourceIDFilter []int64
	filterActive := false
	if rawExts, ok := params["extensions"].([]any); ok && len(rawExts) > 0 {
		var exts []string
		for _, e := range rawExts {
			if s, ok := e.(string); ok {
				exts = append(exts, s)
			}
		}
		if len(exts) > 0 {
			sources, err := h.store.GetDocSourcesByExtensions(exts, docType)
			if err != nil {
				return errorResponse(fmt.Sprintf("resolve sources for extensions %v: %v", exts, err))
			}
			filterActive = true
			for _, src := range sources {
				if project == "" || src.Project == "" || src.Project == project {
					sourceIDFilter = append(sourceIDFilter, src.ID)
				}
			}
		}
	} else if docType != "" {
		// No extensions but doc_type specified — filter all sources by type
		allSources, err := h.store.ListDocSources("")
		if err != nil {
			return errorResponse(fmt.Sprintf("list sources for doc_type %q: %v", docType, err))
		}
		filterActive = true
		for _, src := range allSources {
			if src.DocType == docType {
				if project == "" || src.Project == "" || src.Project == project {
					sourceIDFilter = append(sourceIDFilter, src.ID)
				}
			}
		}
	}

	// Project scope is resolved to source IDs BEFORE the search, so the limit
	// applies to hits from this project (plus global sources). Filtering after
	// the truncation emptied the list whenever a foreign project filled the top
	// of the ranking, and the vector path used to ignore the scope entirely.
	// boostIDs holds the project's own sources (globals are not boosted).
	boostIDs := map[int64]bool{}
	if project != "" {
		scoped, err := h.store.ListDocSourcesForProject(project)
		if err != nil {
			// Never fall back to an unscoped search: the caller asked for this
			// project and a silent widening is indistinguishable from "no hits".
			return errorResponse(fmt.Sprintf("resolve project sources for %q: %v", project, err))
		}
		projectIDs := make([]int64, 0, len(scoped))
		for _, src := range scoped {
			projectIDs = append(projectIDs, src.ID)
			if src.Project != "" {
				boostIDs[src.ID] = true
			}
		}
		if filterActive {
			sourceIDFilter = intersectIDs(sourceIDFilter, projectIDs)
		} else {
			sourceIDFilter = projectIDs
			filterActive = true
		}
	}

	// The filter matched no source at all — no chunk can answer this query.
	if filterActive && len(sourceIDFilter) == 0 {
		return jsonResponse(map[string]any{
			"results": []any{},
			"total":   0,
			"method":  "bm25",
			"message": fmt.Sprintf("Doc search: 0 results for %q", query),
		})
	}

	// BM25 via FTS5 (always)
	bm25Results, err := h.store.SearchDocChunksFTS(query, source, section, since, before, limit*2, sourceIDFilter)
	if err != nil {
		return errorResponse(fmt.Sprintf("doc search failed: %v", err))
	}
	// Normalise to one convention before anything sorts on scores: FTS5's bm25()
	// returns negative values ordered ascending (best = most negative), the
	// vector/RRF paths return positive values where higher is better.
	for i := range bm25Results {
		bm25Results[i].Score = -bm25Results[i].Score
	}

	// Vector search via SSE embeddings. exact=true stays BM25-only: it asks for
	// precise term matching. Every other query runs both paths and merges them,
	// so a semantically matching chunk is found even when BM25 already has hits.
	method := "bm25"
	var vectorResults []storage.DocChunkResult
	if !exact {
		if provider := h.EmbedProvider(); provider != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			vecs, err := provider.Embed(ctx, []string{query})
			if err == nil && len(vecs) > 0 {
				vectorResults, err = h.store.SearchDocChunksSemantic(vecs[0], limit*2, source, sourceIDFilter, since, before)
				if err != nil {
					log.Printf("docs_search: semantic lane: %v", err)
				}
			}
		}
	}

	// Merge: RRF if both have results, otherwise use whichever has results
	var merged []storage.DocChunkResult
	if len(bm25Results) > 0 && len(vectorResults) > 0 {
		method = "hybrid"
		merged = mergeDocResults(bm25Results, vectorResults, limit)
	} else if len(vectorResults) > 0 {
		method = "semantic"
		merged = vectorResults
	} else {
		merged = bm25Results
	}

	// Project boost: lift the project's own sources above the global ones they
	// compete with. Scoping itself already happened in the query above.
	// Scores are normalised (higher = better) at this point, so a plain factor
	// works for every path. boostIDs holds doc_sources.id, the results carry
	// doc_chunks.source_id.
	if len(boostIDs) > 0 {
		for i := range merged {
			if !boostIDs[merged[i].SourceID] {
				continue
			}
			merged[i].Score *= 1.2
		}
		sort.Slice(merged, func(i, j int) bool {
			return merged[i].Score > merged[j].Score
		})
	}

	// Trim to requested limit — after filtering, so the limit counts usable hits.
	if len(merged) > limit {
		merged = merged[:limit]
	}

	// Format results
	type resultItem struct {
		ID          int64   `json:"id"`
		Source      string  `json:"source"`
		Version     string  `json:"version"`
		HeadingPath string  `json:"heading_path"`
		Content     string  `json:"content"`
		Score       float64 `json:"score"`
		SourceFile  string  `json:"source_file"`
		Tokens      int     `json:"tokens_approx"`
	}

	items := make([]resultItem, 0, len(merged))
	for _, r := range merged {
		items = append(items, resultItem{
			ID:          r.ID,
			Source:      r.SourceName,
			Version:     r.Version,
			HeadingPath: r.HeadingPath,
			Content:     r.Content,
			Score:       r.Score,
			SourceFile:  r.SourceFile,
			Tokens:      r.TokensApprox,
		})
	}

	return jsonResponse(map[string]any{
		"results": items,
		"total":   len(items),
		"method":  method,
		"message": fmt.Sprintf("Doc search: %d results for %q", len(items), query),
	})
}

// intersectIDs keeps the values of a that are also present in b. Used when an
// extension/doc_type filter and a project scope are both active.
func intersectIDs(a, b []int64) []int64 {
	keep := make(map[int64]bool, len(b))
	for _, id := range b {
		keep[id] = true
	}
	var out []int64
	for _, id := range a {
		if keep[id] {
			out = append(out, id)
		}
	}
	return out
}

// mergeDocResults performs Reciprocal Rank Fusion on BM25 and vector results.
func mergeDocResults(bm25, vector []storage.DocChunkResult, limit int) []storage.DocChunkResult {
	const k = 60 // RRF constant
	type scored struct {
		result storage.DocChunkResult
		score  float64
	}
	scoreMap := make(map[int64]*scored)

	for rank, r := range bm25 {
		s := 1.0 / float64(k+rank+1)
		if existing, ok := scoreMap[r.ID]; ok {
			existing.score += s
		} else {
			scoreMap[r.ID] = &scored{r, s}
		}
	}
	for rank, r := range vector {
		s := 1.0 / float64(k+rank+1)
		if existing, ok := scoreMap[r.ID]; ok {
			existing.score += s
		} else {
			scoreMap[r.ID] = &scored{r, s}
		}
	}

	all := make([]scored, 0, len(scoreMap))
	for _, s := range scoreMap {
		all = append(all, *s)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		// Equal RRF scores are common; the id keeps the order stable between calls.
		return all[i].result.ID < all[j].result.ID
	})

	if len(all) > limit {
		all = all[:limit]
	}
	results := make([]storage.DocChunkResult, len(all))
	for i, s := range all {
		s.result.Score = s.score
		results[i] = s.result
	}
	return results
}

// handleListDocs lists all registered doc sources.
func (h *Handler) handleListDocs(params map[string]any) Response {
	project := stringOr(params, "project", "")
	sources, err := h.store.ListDocSources(project)
	if err != nil {
		return errorResponse(fmt.Sprintf("list doc sources failed: %v", err))
	}

	type sourceItem struct {
		Name              string `json:"name"`
		Version           string `json:"version"`
		ChunkCount        int    `json:"chunks"`
		Project           string `json:"project"`
		LastSync          string `json:"last_sync"`
		TriggerExtensions string `json:"trigger_extensions,omitempty"`
		Path              string `json:"path,omitempty"`
		PathMissing       bool   `json:"path_missing,omitempty"`
		PathRelative      bool   `json:"path_relative,omitempty"`
	}

	missing := 0
	items := make([]sourceItem, 0, len(sources))
	for _, s := range sources {
		gone := ingest.PathMissing(s.Path)
		if gone {
			missing++
		}
		items = append(items, sourceItem{
			Name:              s.Name,
			Version:           s.Version,
			ChunkCount:        s.ChunkCount,
			Project:           s.Project,
			LastSync:          s.LastSync.Format("2006-01-02 15:04"),
			TriggerExtensions: s.TriggerExtensions,
			Path:              s.Path,
			PathMissing:       gone,
			// A relative path cannot be verified or re-read, so doc-sync skips
			// it forever. It is not "missing" (--prune must not act on it), but
			// it is not healthy either.
			PathRelative: s.Path != "" && !filepath.IsAbs(s.Path),
		})
	}

	var sb strings.Builder
	if len(items) == 0 {
		sb.WriteString("No documentation sources indexed yet.")
	} else {
		sb.WriteString(fmt.Sprintf("%d documentation source(s) indexed.", len(items)))
	}
	if missing > 0 {
		sb.WriteString(fmt.Sprintf(" %d source(s) point at a path that no longer exists; doc-sync skips them.", missing))
	}

	return jsonResponse(map[string]any{
		"sources": items,
		"total":   len(items),
		"message": sb.String(),
	})
}

// handleIngestDocs triggers the doc ingest pipeline via MCP.
// handleRemoveDocs removes a documentation source and all its chunks + learnings.
func (h *Handler) handleRemoveDocs(params map[string]any) Response {
	name := stringOr(params, "name", "")
	if name == "" {
		return errorResponse("name is required")
	}
	// The clone directory is derived from the name and removed recursively.
	if !ingest.ValidSourceName(name) {
		return errorResponse(fmt.Sprintf("invalid doc source name %q", name))
	}
	project := stringOr(params, "project", "")

	// Check if source exists first
	src, err := h.store.GetDocSource(name, project)
	if err != nil || src == nil {
		return errorResponse(fmt.Sprintf("doc source '%s' not found", name))
	}

	chunkCount := src.ChunkCount
	delResult, err := h.store.DeleteDocSource(name, project)
	if err != nil {
		return errorResponse(fmt.Sprintf("remove failed: %v", err))
	}

	learningsDeleted := len(delResult.DeletedLearningIDs)

	// Clean up clone directory
	if delResult != nil && delResult.SourcePath != "" {
		if cloneDir, dirErr := ingest.CloneDir(h.dataDir, name); dirErr != nil {
			log.Printf("warn: skip clone dir cleanup: %q", dirErr)
		} else if _, statErr := os.Stat(cloneDir); statErr == nil {
			os.RemoveAll(cloneDir)
			log.Printf("Clone directory removed: %q", cloneDir)
		}
	}

	// Clean up orphaned embeddings
	if vecStore := h.VectorStore(); vecStore != nil && len(delResult.DeletedLearningIDs) > 0 {
		for _, lid := range delResult.DeletedLearningIDs {
			vecStore.Delete(context.Background(), fmt.Sprintf("l:%d", lid))
		}
	}

	return jsonResponse(map[string]any{
		"message":           fmt.Sprintf("Removed '%s' — %d chunks + %d learnings deleted", name, chunkCount, learningsDeleted),
		"name":              name,
		"chunks_deleted":    chunkCount,
		"learnings_deleted": learningsDeleted,
	})
}

// handleContextualDocs returns top doc chunks from sources matching given file extensions.
// Used by proxy to auto-inject docs when Claude edits specific file types.
func (h *Handler) handleContextualDocs(params map[string]any) Response {
	rawExts, ok := params["extensions"].([]any)
	if !ok || len(rawExts) == 0 {
		return jsonResponse(map[string]any{"results": []any{}, "total": 0})
	}

	var exts []string
	for _, e := range rawExts {
		if s, ok := e.(string); ok {
			exts = append(exts, s)
		}
	}
	if len(exts) == 0 {
		return jsonResponse(map[string]any{"results": []any{}, "total": 0})
	}

	sources, err := h.store.GetDocSourcesByExtensions(exts, "reference")
	if err != nil || len(sources) == 0 {
		return jsonResponse(map[string]any{"results": []any{}, "total": 0})
	}

	project := repo.RootOrSelf(stringOr(params, "project", ""))
	limit := contextualDocsDefaultLimit
	// Read the float directly instead of going through intOr: above MaxInt64 the
	// conversion wraps to a negative value, which no later clamp can tell apart
	// from a caller's negative limit. A negative limit is not a smaller result
	// set either, because SQLite reads LIMIT -1 as "no limit". limit=0 keeps
	// meaning "no results".
	if v, ok := params["limit"].(float64); ok {
		switch {
		case v < 0 || math.IsNaN(v):
			limit = contextualDocsDefaultLimit
		case v > contextualDocsMaxLimit:
			limit = contextualDocsMaxLimit
		default:
			limit = int(v)
		}
	}

	var sourceIDs []int64
	srcNames := make(map[int64]string)
	srcVersions := make(map[int64]string)
	for _, src := range sources {
		if project != "" && src.Project != "" && src.Project != project {
			continue
		}
		sourceIDs = append(sourceIDs, src.ID)
		srcNames[src.ID] = src.Name
		srcVersions[src.ID] = src.Version
	}
	if len(sourceIDs) == 0 {
		return jsonResponse(map[string]any{"results": []any{}, "total": 0})
	}

	chunks, err := h.store.GetDocChunksBySourceIDs(sourceIDs, limit)
	if err != nil {
		return errorResponse(fmt.Sprintf("contextual docs query failed: %v", err))
	}

	type resultItem struct {
		Source      string `json:"source"`
		Version     string `json:"version"`
		HeadingPath string `json:"heading_path"`
		Content     string `json:"content"`
		Tokens      int    `json:"tokens_approx"`
	}

	items := make([]resultItem, 0, len(chunks))
	for _, c := range chunks {
		items = append(items, resultItem{
			Source:      srcNames[c.SourceID],
			Version:     srcVersions[c.SourceID],
			HeadingPath: c.HeadingPath,
			Content:     c.Content,
			Tokens:      c.TokensApprox,
		})
	}

	return jsonResponse(map[string]any{
		"results": items,
		"total":   len(items),
	})
}

// handleListTriggerExtensions returns all unique trigger_extensions across doc sources.
func (h *Handler) handleListTriggerExtensions(params map[string]any) Response {
	project := stringOr(params, "project", "")
	exts, err := h.store.ListTriggerExtensions(project)
	if err != nil {
		return errorResponse(fmt.Sprintf("list trigger extensions: %v", err))
	}
	return jsonResponse(map[string]any{"extensions": exts})
}

func (h *Handler) handleIngestDocs(params map[string]any) Response {
	name := stringOr(params, "name", "")
	if name == "" {
		return errorResponse("name is required")
	}
	// The name becomes a path element on delete, so it must be one from the start.
	if !ingest.ValidSourceName(name) {
		return errorResponse(fmt.Sprintf("invalid doc source name %q", name))
	}
	path := stringOr(params, "path", "")
	if path == "" {
		return errorResponse("path is required")
	}
	// The daemon's working directory is not the caller's, so a relative path
	// here has no base to resolve against and would index whatever tree
	// happens to sit at that name next to the daemon.
	if !filepath.IsAbs(path) {
		return errorResponse(fmt.Sprintf("path %q must be absolute", path))
	}
	version := stringOr(params, "version", "")
	project := stringOr(params, "project", "")
	domain := stringOr(params, "domain", "code")
	docType := stringOr(params, "doc_type", "")

	rules, _ := params["rules"].(bool)

	// Parse trigger_extensions: accepts string (".twig,.html.twig") or array [".twig", ".html.twig"].
	// Absent → preserves existing value. Empty string/array → clears via sentinel "-".
	var triggerExts string
	if rawStr, ok := params["trigger_extensions"].(string); ok {
		if rawStr == "" {
			triggerExts = "-"
		} else {
			triggerExts = rawStr
		}
	} else if rawExts, ok := params["trigger_extensions"].([]any); ok {
		var extParts []string
		for _, e := range rawExts {
			if s, ok := e.(string); ok {
				extParts = append(extParts, s)
			}
		}
		if len(extParts) == 0 {
			triggerExts = "-"
		} else {
			triggerExts = strings.Join(extParts, ",")
		}
	}

	// Rules path: condense CLAUDE.md into a rules block for periodic re-injection
	if rules {
		return h.handleIngestRules(name, path, project)
	}
	cfg := ingest.Config{
		Name:              name,
		Version:           version,
		Project:           project,
		Domain:            domain,
		TriggerExtensions: triggerExts,
		DocType:           docType,
	}

	// Wire LLM client for destillation
	if summarize := h.summarizeLLM(); cfg.Destill && summarize != nil {
		cfg.LLMClient = summarize
	} else if cfg.Destill {
		log.Printf("  warn: --destill requested but no LLM client available, skipping destillation")
		cfg.Destill = false
	}

	result, err := ingest.Run(cfg, []string{path}, h.store)
	if err != nil {
		return errorResponse(fmt.Sprintf("ingest failed: %v", err))
	}

	// Embed new chunks in background
	if result.ChunksCreated > 0 {
		go h.EmbedDocChunks()
	}

	return jsonResponse(map[string]any{
		"message": fmt.Sprintf("Ingest complete: %d files processed, %d skipped, %d removed, %d chunks created, %d unreadable entries skipped",
			result.FilesProcessed, result.FilesSkipped, result.FilesRemoved, result.ChunksCreated, result.UnreadableEntries),
		"files_processed":      result.FilesProcessed,
		"files_skipped":        result.FilesSkipped,
		"files_removed":        result.FilesRemoved,
		"unreadable_entries":   result.UnreadableEntries,
		"chunks_created":       result.ChunksCreated,
		"learnings_superseded": result.LearningsSuperseded,
	})
}

// handleIngestRules condenses a CLAUDE.md (with link resolution) into a rules block.
func (h *Handler) handleIngestRules(name, sourcePath, project string) Response {
	// Prefer quality client (Sonnet/Opus) for rules — Haiku hallucinates generic rules
	client := h.qualityLLMClient()
	if client == nil {
		client = h.summarizeLLM()
	}
	if client == nil {
		client = h.commitEvalLLM()
	}
	if client == nil {
		return errorResponse("no LLM client available for rules condensation — run extraction cycle first or check API key config")
	}

	condensed, hash, err := CondenseRules(sourcePath, project, client, h.store)
	if err != nil {
		if err.Error() == "unchanged" {
			return jsonResponse(map[string]any{
				"message": "Rules unchanged (same hash), skipped condensation",
				"hash":    h.store.GetRulesHash(project),
			})
		}
		return errorResponse(err.Error())
	}

	log.Printf("[rules] condensed %q → %d chars (hash=%s)", sourcePath, len(condensed), hash[:12])
	return jsonResponse(map[string]any{
		"message":       fmt.Sprintf("Rules condensed: %d chars from %s", len(condensed), filepath.Base(sourcePath)),
		"hash":          hash,
		"condensed_len": len(condensed),
		"source_path":   sourcePath,
	})
}

// CondenseRules reads a CLAUDE.md, resolves links, checks hash, and condenses via LLM.
// Returns (condensed, hash, error). Returns error "unchanged" if the source hash
// matches AND a non-empty rules block is already stored.
func CondenseRules(sourcePath, project string, client extraction.LLMClient, store *storage.Store) (string, string, error) {
	expanded, err := expandWithLinks(sourcePath, 0)
	if err != nil {
		return "", "", fmt.Errorf("read rules source: %v", err)
	}

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(expanded)))
	// The hash alone is not enough: a rules row whose content was cleared keeps
	// its hash, and the shortcut would leave the block empty forever. Only skip
	// when there is an actual block to reuse.
	if existing := store.GetRulesHash(project); existing == hash && strings.TrimSpace(store.GetRulesContent(project)) != "" {
		return "", hash, fmt.Errorf("unchanged")
	}

	// Step 1: Mechanical pre-filter — remove code blocks, tables, directory trees.
	// These are never behavioral rules. Keeps prose, bullets, headings.
	filtered := mechanicalPreFilter(expanded)

	// Guard: cap filtered input to prevent runaway LLM costs on large CLAUDE.mds
	// with many linked files. 50k chars ≈ 12k tokens input to Sonnet.
	const maxFilteredChars = 50000
	if len(filtered) > maxFilteredChars {
		filtered = filtered[:maxFilteredChars]
	}

	// Step 2: Single LLM call to extract and condense rules, conventions, principles.
	// Using quality model (Sonnet/Opus) — Haiku hallucinates generic rules.
	condensePrompt := `You are a rules extractor for an AI coding assistant. The input has been pre-filtered to remove code blocks, tables, directory trees, and reference sections. Everything remaining is potentially relevant — err on the side of including too much rather than missing behavioral guidance.

EXTRACT (keep original meaning, condense wording):
- Hard rules: "Never X", "Always Y", "Do not Z"
- Conventions: naming, language, patterns ("Queries in German", "BM25 with AND-matching")
- Principles: design philosophy ("Quality over cost", "Memory as agent not for agent")
- Gotchas: things that break if done wrong ("additionalProperties: false on ALL nested objects")
- Process requirements: workflows, review steps, deployment procedures
- Design targets: numerical goals that guide tradeoffs ("Target ~130k tokens")
- Known traps: concurrency issues, API quirks, data integrity constraints

DO NOT EXTRACT:
- Feature completion status ("Phase X LIVE", "merged to main", "commit abc123")
- Pure metrics without actionable context ("save_rate 62%", "3.2% impact")
- Tool/CLI reference lists

Output format: grouped by theme, imperative or descriptive form as appropriate.
Include a brief WHY when the source provides reasoning.
Be thorough — include ALL behavioral guidance, but CONDENSE aggressively. Use terse imperative form.
The output must be significantly shorter than the input — aim for 40-60% reduction.
Same language as input. No preamble, no introduction.`
	condenseUser := fmt.Sprintf("Extract all behavioral guidance from this documentation:\n\n%s", filtered)

	condensed, err := client.Complete(condensePrompt, condenseUser)
	if err != nil {
		return "", "", fmt.Errorf("LLM condensation failed: %v", err)
	}
	// An empty answer must not be stored: it would blank the rules block and
	// record the fresh hash, so no later run would ever re-condense it.
	if strings.TrimSpace(condensed) == "" {
		return "", "", fmt.Errorf("LLM returned an empty rules condensation — stored block kept")
	}

	ds := &storage.DocSource{
		Name:        "rules",
		Path:        sourcePath,
		Project:     project,
		IsSkill:     false,
		FullContent: condensed,
	}
	sourceID, err := store.UpsertDocSource(ds)
	if err != nil {
		return "", "", fmt.Errorf("upsert doc_source: %v", err)
	}
	if err := store.SaveRulesContent(sourceID, condensed, hash); err != nil {
		return "", "", fmt.Errorf("save rules content: %v", err)
	}

	return condensed, hash, nil
}

// expandWithLinks reads a file and recursively expands @path references (depth max 2).
func expandWithLinks(filePath string, depth int) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	if depth >= 2 {
		return string(data), nil
	}
	dir := filepath.Dir(filePath)
	var result strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "@") {
			ref := strings.TrimPrefix(trimmed, "@")
			refPath := filepath.Join(dir, ref)
			if isTextFile(refPath) {
				expanded, err := expandWithLinks(refPath, depth+1)
				if err == nil {
					result.WriteString(expanded)
					result.WriteByte('\n')
					continue
				}
			}
		}
		result.WriteString(line)
		result.WriteByte('\n')
	}
	return result.String(), nil
}

// mechanicalPreFilter removes code blocks, tables, directory trees, reference sections,
// and other non-behavioral content from markdown. Heading-aware: skips entire sections
// whose heading signals reference/listing content. Keeps prose, bullets, headings that
// contain behavioral guidance.
// This is a deterministic pre-processing step — no LLM needed.
func mechanicalPreFilter(input string) string {
	var result strings.Builder
	lines := strings.Split(input, "\n")
	inCodeBlock := false
	inTable := false
	skipSection := false  // true when current section heading signals non-behavioral content
	skipSectionLevel := 0 // heading level that triggered skip (only same or higher level ends it)
	skipIndented := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Toggle code blocks
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		// Heading detection — controls section-level skip
		if strings.HasPrefix(trimmed, "#") {
			level := 0
			for _, c := range trimmed {
				if c == '#' {
					level++
				} else {
					break
				}
			}
			headingText := strings.ToLower(strings.TrimSpace(trimmed[level:]))

			// End section skip if we hit a heading at same or higher level
			if skipSection && level <= skipSectionLevel {
				skipSection = false
			}

			// Check if this heading signals a reference/listing section
			if isReferenceHeading(headingText) {
				skipSection = true
				skipSectionLevel = level
				continue
			}

			// Keep the heading
			inTable = false
			skipIndented = 0
			result.WriteString(line)
			result.WriteByte('\n')
			continue
		}

		// Skip entire reference sections
		if skipSection {
			continue
		}

		// Skip table rows (pipes)
		if strings.Contains(trimmed, "|") && strings.Count(trimmed, "|") >= 2 {
			inTable = true
			continue
		}
		if inTable && trimmed == "" {
			inTable = false
			continue
		}
		if inTable {
			continue
		}

		// Skip directory tree lines
		if strings.ContainsAny(trimmed, "├└│") || strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../") {
			skipIndented++
			continue
		}
		// Skip runs of heavily indented lines (likely directory listings or formatted output)
		// but preserve bullet points
		if len(line) > 0 && (line[0] == '\t' || strings.HasPrefix(line, "    ")) && !strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "*") {
			skipIndented++
			if skipIndented > 3 {
				continue
			}
		} else {
			skipIndented = 0
		}

		// Skip lines that are just file paths, URLs, or bare key:value config
		if isNonBehavioralLine(trimmed) {
			continue
		}

		// Keep everything else
		if trimmed == "" {
			result.WriteByte('\n')
			continue
		}

		result.WriteString(line)
		result.WriteByte('\n')
	}
	return result.String()
}

// isReferenceHeading returns true if a heading signals a reference/listing section
// that never contains behavioral guidance. Matches common CLAUDE.md patterns across projects.
func isReferenceHeading(heading string) bool {
	// Exact patterns that are always reference content
	referencePatterns := []string{
		"schema", "reference", "api endpoint", "cli command", "mcp tool",
		"database", "rpc method", "internal package", "file structure",
		"directory", "route", "endpoint", "table", "migration",
	}
	for _, p := range referencePatterns {
		if strings.Contains(heading, p) {
			return true
		}
	}
	return false
}

// isNonBehavioralLine returns true for lines that are pure data, not guidance.
func isNonBehavioralLine(line string) bool {
	// Bare file paths
	if strings.HasPrefix(line, "/") && !strings.Contains(line, " ") {
		return true
	}
	// URLs without context
	if (strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")) && !strings.Contains(line, " ") {
		return true
	}
	// Lines that are just a command name + description (CLI reference patterns)
	// e.g. "  build          Build binary → ./yesmem"
	if len(line) > 2 && line[0] == ' ' && strings.Contains(line, "  ") {
		parts := strings.Fields(line)
		if len(parts) == 2 && !strings.ContainsAny(parts[0], ".-()") {
			return false // Could be a bullet point abbreviation
		}
	}
	return false
}

func isTextFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md", ".txt", ".rst", ".yaml", ".yml", ".toml":
		return true
	}
	return false
}

// handleGetRulesBlock returns the condensed rules block for proxy re-injection.
func (h *Handler) handleGetRulesBlock(params map[string]any) Response {
	project, _ := params["project"].(string)
	content := h.store.GetRulesContent(project)
	if content == "" {
		return jsonResponse(map[string]any{"content": "", "exists": false})
	}
	return jsonResponse(map[string]any{"content": content, "exists": true})
}

// EmbedDocChunks embeds all doc chunks that don't have embeddings yet.
// Called at daemon startup and after ingest. Mutex prevents parallel runs.
func (h *Handler) EmbedDocChunks() {
	embedDocChunksMu.Lock()
	defer embedDocChunksMu.Unlock()

	provider := h.EmbedProvider()
	if provider == nil {
		return
	}

	chunks, err := h.store.DocChunksWithoutEmbedding()
	if err != nil {
		log.Printf("embed doc chunks: list failed: %v", err)
		return
	}
	if len(chunks) == 0 {
		return
	}

	log.Printf("embed doc chunks: %d chunks to embed", len(chunks))

	// Batch embed (32 at a time)
	const batchSize = 32
	embedded := 0
	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[i:end]

		texts := make([]string, len(batch))
		for j, c := range batch {
			texts[j] = c.HeadingPath + " " + c.Content
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		vecs, err := provider.Embed(ctx, texts)
		cancel()
		if err != nil {
			log.Printf("embed doc chunks: batch %d-%d failed: %v", i, end, err)
			continue
		}

		for j, c := range batch {
			if j >= len(vecs) {
				break
			}
			if err := h.store.SetDocChunkEmbedding(c.ID, vecs[j], c.ContentHash); err != nil {
				log.Printf("embed doc chunks: store chunk %d failed: %v", c.ID, err)
			} else {
				embedded++
			}
		}
	}

	log.Printf("embed doc chunks: %d/%d embedded", embedded, len(chunks))
}

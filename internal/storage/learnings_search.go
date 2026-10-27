package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/carsteneu/yesmem/internal/models"
)

// projectMatchesTolerant reports whether a learning (identified by its per-row project
// and canonical_project columns) belongs to the requested project. Tolerance is needed
// because callers pass either short names ("yesmem"), full paths ("/home/x/yesmem"), or
// worktree paths ("/home/x/yesmem/.worktrees/foo"), while learnings carry a mix of
// canonical_project values ("yesmem" for legacy rows, full paths for newer rows).
//
// Comparison rules:
//   - Empty requested matches anything (no filter applied).
//   - Empty stored values (legacy rows without project/canonical_project) match any
//     requested value so these rows remain discoverable, mirroring the
//     `OR canonical_project = ”` clause in SearchUnfinished.
//   - Direct equality on either stored field (canonical or raw) matches.
//   - A row that names its repository by PATH is compared through that path: two paths
//     describe the same project when their cluster keys agree, which folds the worktrees
//     of a repository onto its root. Two repositories that share a basename keep separate
//     keys, so they stay apart even though CanonicalProject is equal for both (YM-424).
//   - A row WITHOUT a path (a legacy bare name) has no other handle than its name, so it
//     stays discoverable under any request that carries the same basename. That is the
//     deliberate difference between the legacy and the unassigned state: an unassigned
//     row matches every request, a legacy row only a same-named one.
func projectMatchesTolerant(storedProject, storedCanonical, requested string) bool {
	if requested == "" {
		return true
	}
	if storedCanonical == "" && storedProject == "" {
		return true
	}
	if storedProject != "" && storedProject == requested {
		return true
	}
	if storedCanonical != "" && storedCanonical == requested {
		return true
	}
	reqCanon := models.CanonicalProject(requested)
	if models.ClassifyProject(storedProject) == models.ProjectOriginPath {
		if strings.HasPrefix(requested, "/") {
			key := models.ClusterProjectKey(storedProject)
			return key != "" && key == models.ClusterProjectKey(requested)
		}
		return models.CanonicalProject(storedProject) == reqCanon
	}
	return models.CanonicalProject(storedCanonical) == reqCanon ||
		models.CanonicalProject(storedProject) == reqCanon
}

// QueryFactsOpts defines filters for structured fact queries on learning metadata.
type QueryFactsOpts struct {
	Entity   string // LIKE match on learning_entities.value
	Action   string // LIKE match on learning_actions.value
	Keyword  string // LIKE match on learning_keywords.value
	Domain   string // Exact match on learnings.domain
	Project  string // Project filter
	Category string // Category filter
	Limit    int    // Default 20
}

// QueryFacts returns active learnings matching structured metadata filters (entities, actions, keywords).
func (s *Store) QueryFacts(opts QueryFactsOpts) ([]models.Learning, error) {
	if opts.Limit <= 0 {
		opts.Limit = 20
	}

	selectCols := `l.id, l.session_id, l.category, l.content, l.project, l.confidence,
		l.superseded_by, l.supersede_reason, l.created_at, l.expires_at, l.model_used, l.source,
		COALESCE(l.hit_count, 0), COALESCE(l.emotional_intensity, 0.0), l.last_hit_at, COALESCE(l.session_flavor, ''), l.valid_until, l.supersedes, COALESCE(l.importance, 3), l.supersede_status, COALESCE(l.noise_count, 0), COALESCE(l.fail_count, 0),
		COALESCE(l.match_count, 0), COALESCE(l.inject_count, 0), COALESCE(l.use_count, 0), COALESCE(l.save_count, 0), COALESCE(l.stability, 30.0),
		COALESCE(l.context, ''), COALESCE(l.domain, 'code'), COALESCE(l.trigger_rule, ''), COALESCE(l.embedding_text, ''),
		COALESCE(l.source_file, ''), COALESCE(l.source_hash, ''), COALESCE(l.doc_chunk_ref, 0), COALESCE(l.task_type, ''), COALESCE(l.turns_at_creation, 0), COALESCE(l.origin_tool, ''), COALESCE(l.source_msg_from, -1), COALESCE(l.source_msg_to, -1),
		COALESCE(l.canonical_project, ''),
		COALESCE(l.attribution, ''),
		COALESCE(l.staleness_score, 0.0), COALESCE(l.staleness_reason, ''), l.staleness_checked_at, COALESCE(l.staleness_type, ''), COALESCE(l.code_fingerprint, ''), COALESCE(l.project_source, '')`

	query := `SELECT DISTINCT ` + selectCols + ` FROM learnings l`
	var joins []string
	var where []string
	var args []any

	where = append(where, models.RecallableLearningSQLAlias)
	where = append(where, `(l.expires_at IS NULL OR julianday(l.expires_at) > julianday(?))`)
	args = append(args, fmtTime(time.Now()))

	if opts.Entity != "" {
		joins = append(joins, `JOIN learning_entities le ON le.learning_id = l.id`)
		where = append(where, `le.value LIKE ?`)
		args = append(args, "%"+opts.Entity+"%")
	}
	if opts.Action != "" {
		joins = append(joins, `JOIN learning_actions la ON la.learning_id = l.id`)
		where = append(where, `la.value LIKE ?`)
		args = append(args, "%"+opts.Action+"%")
	}
	if opts.Keyword != "" {
		joins = append(joins, `JOIN learning_keywords lk ON lk.learning_id = l.id`)
		where = append(where, `lk.value LIKE ?`)
		args = append(args, "%"+opts.Keyword+"%")
	}
	if opts.Domain != "" {
		where = append(where, `l.domain = ?`)
		args = append(args, opts.Domain)
	}
	if opts.Project != "" {
		where = append(where, `l.canonical_project = ?`)
		args = append(args, s.resolveCanonicalProject(opts.Project))
	}
	if opts.Category != "" {
		where = append(where, `l.category = ?`)
		args = append(args, opts.Category)
	}

	for _, j := range joins {
		query += " " + j
	}
	query += " WHERE " + strings.Join(where, " AND ")
	query += ` ORDER BY l.importance DESC, l.created_at DESC LIMIT ?`
	args = append(args, opts.Limit)

	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query facts: %w", err)
	}
	defer rows.Close()

	return scanLearnings(rows)
}

// MinResolveTerms is the number of query words a text search must carry before
// an unfinished item may be resolved without an explicit id. Below that, the
// overlap is too likely to come from a coincidence shared with an unrelated item.
const MinResolveTerms = 3

// maxUnfinishedQueryTerms caps the AND tier width so a pasted paragraph cannot
// turn one search into dozens of FTS queries.
const maxUnfinishedQueryTerms = 10

// UnfinishedSearch is the result of a ranked unfinished-item search.
//
// Items holds candidates, best first: the hits of the first AND tier that
// produced any, or, when no tier found anything, the ranked hits of a
// single-word fallback (MatchedTerms == 0). Terms is the number of distinct
// words in the query, MatchedTerms the number of words the winning tier
// required. Candidates from a relaxed tier may still contain some of the
// dropped words, so Items must not be read as "contains every term" unless
// FullMatch reports true.
type UnfinishedSearch struct {
	Items        []models.Learning
	Terms        int
	MatchedTerms int
	// IndexedTerms counts the query words that occur anywhere in the knowledge
	// base. Words below Terms can never be matched, so they rule out a full match.
	IndexedTerms int
}

// FullMatch reports whether the winning tier required every query word.
func (r *UnfinishedSearch) FullMatch() bool {
	return r != nil && r.Terms > 0 && r.MatchedTerms == r.Terms && len(r.Items) > 0
}

// SkipReason explains why nothing may be resolved automatically. It is empty
// exactly when AutoResolveTarget succeeds.
func (r *UnfinishedSearch) SkipReason() string {
	switch {
	case r == nil || r.Terms == 0:
		return "empty search text"
	case len(r.Items) == 0:
		return "no matching unfinished items found"
	case r.Terms < MinResolveTerms:
		return fmt.Sprintf("search text has %d term(s), at least %d are required to resolve without an explicit id", r.Terms, MinResolveTerms)
	case r.Terms > maxUnfinishedQueryTerms:
		return fmt.Sprintf("search text has %d terms, only the first %d are searched; pass a shorter distinctive phrase", r.Terms, maxUnfinishedQueryTerms)
	case r.IndexedTerms < r.Terms:
		return fmt.Sprintf("%d of %d search terms occur in no learning at all, so no unfinished item can contain them all", r.Terms-r.IndexedTerms, r.Terms)
	case !r.FullMatch():
		if r.MatchedTerms == 0 {
			return fmt.Sprintf("no unfinished item contains all %d search terms, the candidates below only share single words", r.Terms)
		}
		return fmt.Sprintf("no unfinished item contains all %d search terms (closest candidates match %d of them)", r.Terms, r.MatchedTerms)
	case len(r.Items) > 1:
		return fmt.Sprintf("%d unfinished items contain all %d search terms, pass the learning_id instead", len(r.Items), r.Terms)
	}
	return ""
}

// AutoResolveTarget returns the only item that may be resolved without an
// explicit id: a unique candidate that contains every query word, with the query
// carrying at least MinResolveTerms words.
func (r *UnfinishedSearch) AutoResolveTarget() (models.Learning, bool) {
	if r == nil || r.SkipReason() != "" {
		return models.Learning{}, false
	}
	return r.Items[0], true
}

// SearchUnfinished searches active unfinished items using FTS5 full-text search.
// The result is a display list: when no conjunction matched, it can contain items
// that share only a single query word. Resolving decisions must use
// SearchUnfinishedRanked (resolve_by_text) or ResolveCommit (commit subjects);
// neither this function nor a plain first hit may close an item.
func (s *Store) SearchUnfinished(query, project string) ([]models.Learning, error) {
	found, err := s.SearchUnfinishedRanked(query, project)
	if err != nil {
		return nil, err
	}
	return found.Items, nil
}

// SearchUnfinishedRanked searches active unfinished items with AND matching and
// reports how complete the match is.
//
// The query words are joined with AND, never OR: the project convention is BM25
// over all terms. When the full conjunction finds nothing, the search relaxes by
// dropping the most common word first (lowest IDF) until a tier produces hits.
// That order keeps a single shared word from outranking a document that contains
// the whole query, which is how learning #66092 got resolved by mistake.
func (s *Store) SearchUnfinishedRanked(query, project string) (*UnfinishedSearch, error) {
	words := splitWords(query)
	if len(words) == 0 {
		return &UnfinishedSearch{}, nil
	}
	// Punctuation-only tokens have no FTS5 tokens at all and would make the MATCH
	// fail with a syntax error; they cannot carry a match either way.
	searchable := make([]string, 0, len(words))
	keys := make([]string, 0, len(words))
	for _, w := range words {
		key := searchKey(w)
		if key == "" {
			continue
		}
		searchable = append(searchable, w)
		keys = append(keys, key)
	}
	// Repeated words count once, keyed on the alphanumeric core so that "proxy,",
	// "proxy." and "proxy" cannot pose as three terms and satisfy the three-word
	// minimum with a single one.
	seen := make(map[string]bool, len(keys))
	words = searchable[:0]
	for i, w := range searchable {
		if !seen[keys[i]] {
			seen[keys[i]] = true
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return &UnfinishedSearch{}, nil
	}
	// Count before the cap: a long text that was truncated must never look like a
	// full match, otherwise the dropped words would be silently unchecked.
	totalTerms := len(words)
	if len(words) > maxUnfinishedQueryTerms {
		words = words[:maxUnfinishedQueryTerms]
	}

	// Quoting prevents FTS5 from reading hyphens as column operators.
	type termFreq struct {
		term  string
		count int
	}
	var freqs []termFreq
	for _, w := range words {
		term := `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
		var cnt int
		if err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM learnings_fts WHERE learnings_fts MATCH ?`, term).Scan(&cnt); err != nil {
			return nil, fmt.Errorf("search unfinished term frequency: %w", err)
		}
		if cnt > 0 {
			freqs = append(freqs, termFreq{term: term, count: cnt})
		}
	}
	sort.Slice(freqs, func(i, j int) bool { return freqs[i].count < freqs[j].count })

	found := &UnfinishedSearch{Terms: totalTerms, IndexedTerms: len(freqs)}
	for width := len(freqs); width >= 1; width-- {
		terms := make([]string, width)
		for i := 0; i < width; i++ {
			terms[i] = freqs[i].term
		}
		items, err := s.searchUnfinishedTier(terms, project, " AND ")
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			found.Items = items
			found.MatchedTerms = width
			return found, nil
		}
	}

	// Nothing matched a conjunction. Still return the items that share a single
	// word so the caller can list them as candidates: the AND tiers only ever try
	// the rarest words first, and in a small project those can be the ones whose
	// open tasks are gone. This fallback is display-only, never a resolve basis
	// (MatchedTerms == 0 keeps FullMatch false).
	if len(freqs) > 0 {
		orTerms := make([]string, len(freqs))
		for i, f := range freqs {
			orTerms[i] = f.term
		}
		items, err := s.searchUnfinishedTier(orTerms, project, " OR ")
		if err != nil {
			return nil, err
		}
		found.Items = items
	}
	return found, nil
}

// searchKey reduces a query word to its alphanumeric core, which is what the
// FTS5 tokenizer makes of it. An empty key means the word carries no token.
func searchKey(w string) string {
	var b strings.Builder
	b.Grow(len(w))
	for _, r := range w {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// unfinishedSelectColumns is the scan order shared by every unfinished-item
// query: scanLearnings expects exactly this column list.
const unfinishedSelectColumns = `l.id, l.session_id, l.category, l.content, l.project, l.confidence,
		l.superseded_by, l.supersede_reason, l.created_at, l.expires_at, l.model_used, l.source,
		COALESCE(l.hit_count, 0), COALESCE(l.emotional_intensity, 0.0), l.last_hit_at, COALESCE(l.session_flavor, ''), l.valid_until, l.supersedes, COALESCE(l.importance, 3), l.supersede_status, COALESCE(l.noise_count, 0), COALESCE(l.fail_count, 0),
		COALESCE(l.match_count, 0), COALESCE(l.inject_count, 0), COALESCE(l.use_count, 0), COALESCE(l.save_count, 0), COALESCE(l.stability, 30.0),
		COALESCE(l.context, ''), COALESCE(l.domain, 'code'), COALESCE(l.trigger_rule, ''), COALESCE(l.embedding_text, ''),
		COALESCE(l.source_file, ''), COALESCE(l.source_hash, ''), COALESCE(l.doc_chunk_ref, 0), COALESCE(l.task_type, ''), COALESCE(l.turns_at_creation, 0), COALESCE(l.origin_tool, ''), COALESCE(l.source_msg_from, -1), COALESCE(l.source_msg_to, -1),
		COALESCE(l.canonical_project, ''),
		COALESCE(l.attribution, ''),
		COALESCE(l.staleness_score, 0.0), COALESCE(l.staleness_reason, ''), l.staleness_checked_at, COALESCE(l.staleness_type, ''), COALESCE(l.code_fingerprint, ''), COALESCE(l.project_source, '')`

// searchUnfinishedTier runs one tier of the unfinished search. joiner is
// " AND " for a real tier and " OR " only for the display-only candidate
// fallback.
func (s *Store) searchUnfinishedTier(terms []string, project, joiner string) ([]models.Learning, error) {
	base := `SELECT ` + unfinishedSelectColumns + `
		FROM learnings_fts
		JOIN learnings l ON l.id = learnings_fts.rowid
		WHERE learnings_fts MATCH ?
		AND ` + models.RecallableLearningSQLAlias + ` AND l.category = 'unfinished'`
	args := []any{strings.Join(terms, joiner)}

	if project != "" {
		base += ` AND (l.canonical_project = ? OR l.canonical_project = '')`
		args = append(args, s.resolveCanonicalProject(project))
	}

	base += ` ORDER BY bm25(learnings_fts) LIMIT 10`

	rows, err := s.readerDB().Query(base, args...)
	if err != nil {
		return nil, fmt.Errorf("search unfinished: %w", err)
	}
	defer rows.Close()
	return scanLearnings(rows)
}

// LearningSearchResult is a lightweight result for hybrid search fusion.
type LearningSearchResult struct {
	ID      string
	Content string
	Score   float64
	Project string
}

// SearchLearningsBM25 searches active learnings using FTS5 and returns ranked results
// with BM25 scores. Designed for RRF fusion with vector search (same ID space).
func (s *Store) SearchLearningsBM25(query, project string, limit int) ([]LearningSearchResult, error) {
	return s.SearchLearningsBM25Ctx(context.Background(), query, project, "", "", limit)
}

// SearchLearningsBM25Ctx is like SearchLearningsBM25 but accepts a context for cancellation/timeout.
// Uses a tiered AND-query approach:
//   - Tier 1 (100% terms AND) → score max 100
//   - Tier 2 (66% terms AND) → score max 60
//   - Tier 3 (33% terms AND) → score max 40
func (s *Store) SearchLearningsBM25Ctx(ctx context.Context, query, project, since, before string, limit int) ([]LearningSearchResult, error) {
	words := splitWords(query)
	if len(words) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 3
	}
	if err := validateFilterBounds(since, before); err != nil {
		return nil, err
	}

	// Quote each word for FTS5
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}

	type ftsHit struct {
		id      string
		content string
		score   float64
	}

	runFTS := func(terms []string) ([]ftsHit, error) {
		ftsQuery := strings.Join(terms, " AND ")
		ftsSQL := `SELECT rowid, content, bm25(learnings_fts) AS score FROM learnings_fts WHERE learnings_fts MATCH ? ORDER BY bm25(learnings_fts) LIMIT ?`
		rows, err := s.readerDB().QueryContext(ctx, ftsSQL, ftsQuery, limit*5)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var hits []ftsHit
		for rows.Next() {
			var h ftsHit
			if rows.Scan(&h.id, &h.content, &h.score) == nil {
				h.score = -h.score // FTS5 bm25() is negative (lower=better), negate
				hits = append(hits, h)
			}
		}
		return hits, rows.Err()
	}

	// Tiered AND search: cap at 5, 4, 3, 2 terms.
	// Terms sorted by IDF (rarest first) so when we drop for lower tiers,
	// we lose the most common (least specific) terms first.
	// Scores: 5 terms = 100, 4 terms = 90, 3 terms = 70, 2 terms = 50.

	// IDF-sort: count document frequency per term, filter out zero-hit terms, sort rarest first
	type termFreq struct {
		term  string
		count int
	}
	var (
		freqs  []termFreq
		freqMu sync.Mutex
		wg     sync.WaitGroup
		sem    = make(chan struct{}, 4)
	)
	for _, q := range quoted {
		wg.Add(1)
		sem <- struct{}{}
		go func(term string) {
			defer wg.Done()
			defer func() { <-sem }()
			var cnt int
			_ = s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM learnings_fts WHERE learnings_fts MATCH ?`, term).Scan(&cnt)
			if cnt > 0 {
				freqMu.Lock()
				freqs = append(freqs, termFreq{term, cnt})
				freqMu.Unlock()
			}
		}(q)
	}
	wg.Wait()
	if len(freqs) == 0 {
		return nil, nil
	}
	sort.Slice(freqs, func(i, j int) bool {
		return freqs[i].count < freqs[j].count
	})
	sortedTerms := make([]string, len(freqs))
	for i, f := range freqs {
		sortedTerms[i] = f.term
	}

	type tier struct {
		terms    []string
		maxScore float64
	}
	n := len(sortedTerms)
	seen := make(map[int]bool)
	var tiers []tier
	for _, count := range []int{5, 4, 3, 2, 1} {
		c := count
		if c > n {
			c = n
		}
		if c < 1 || seen[c] {
			continue
		}
		seen[c] = true
		score := map[int]float64{5: 100, 4: 90, 3: 70, 2: 50, 1: 30}[count]
		if c < count {
			// fewer terms available than tier target — downgrade score proportionally
			score = score * float64(c) / float64(count)
		}
		tiers = append(tiers, tier{sortedTerms[:c], score})
	}

	var hits []ftsHit
	var maxScore float64
	for _, t := range tiers {
		h, err := runFTS(t.terms)
		if err != nil {
			return nil, fmt.Errorf("search learnings bm25 tier: %w", err)
		}
		if len(h) > 0 {
			hits = h
			maxScore = t.maxScore
			break
		}
	}
	if len(hits) == 0 {
		return nil, nil
	}

	// Normalize BM25 scores within tier: top hit = maxScore, others scale proportionally
	topBM25 := hits[0].score
	if topBM25 > 0 {
		for i := range hits {
			hits[i].score = (hits[i].score / topBM25) * maxScore
		}
	}

	// Step 2: Metadata lookup for filtering
	placeholders := make([]string, len(hits))
	args := make([]any, len(hits))
	for i, h := range hits {
		placeholders[i] = "?"
		args[i] = h.id
	}

	metaSQL := `SELECT CAST(id AS TEXT), COALESCE(project, ''), COALESCE(canonical_project, ''), category, created_at FROM learnings WHERE id IN (` + strings.Join(placeholders, ",") + `) AND ` + models.RecallableLearningSQL
	metaRows, err := s.readerDB().QueryContext(ctx, metaSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("search learnings meta: %w", err)
	}

	type meta struct {
		project          string
		canonicalProject string
		category         string
		createdAt        string
	}
	metaMap := make(map[string]meta, len(hits))
	for metaRows.Next() {
		var id, proj, canon, cat, created string
		if metaRows.Scan(&id, &proj, &canon, &cat, &created) == nil {
			metaMap[id] = meta{project: proj, canonicalProject: canon, category: cat, createdAt: created}
		}
	}
	metaRows.Close()

	// Step 3: Merge and filter
	// Compare parsed instants: created_at carries the writer's offset, so a
	// string comparison against a UTC or date-only bound shifts the window.
	sinceT := ParseFilterTime(since)
	beforeT := ParseFilterTime(before)
	var results []LearningSearchResult
	for _, h := range hits {
		m, ok := metaMap[h.id]
		if !ok {
			continue
		}
		score := h.score
		switch m.category {
		case "narrative":
			score *= 0.4
		case "unfinished":
			score *= 0.7
		}
		if project != "" && !projectMatchesTolerant(m.project, m.canonicalProject, project) {
			continue
		}
		createdT := ParseTimestamp(m.createdAt)
		if !sinceT.IsZero() && createdT.Before(sinceT) {
			continue
		}
		if !beforeT.IsZero() && !createdT.Before(beforeT) {
			continue
		}
		results = append(results, LearningSearchResult{
			ID:      h.id,
			Content: h.content,
			Score:   score,
			Project: m.project,
		})
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

// FindLearningsByEntityMatch finds active learnings whose entities match the given values.
// Uses the learning_entities junction table for lookup, returns full Learning structs via scanLearnings.
func (s *Store) FindLearningsByEntityMatch(entities []string, project string) ([]models.Learning, error) {
	if len(entities) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(entities))
	args := make([]any, len(entities))
	for i, e := range entities {
		placeholders[i] = "?"
		args[i] = e
	}

	query := `SELECT DISTINCT l.id, l.session_id, l.category, l.content, l.project, l.confidence,
		l.superseded_by, l.supersede_reason, l.created_at, l.expires_at, l.model_used, l.source,
		COALESCE(l.hit_count, 0), COALESCE(l.emotional_intensity, 0.0), l.last_hit_at, COALESCE(l.session_flavor, ''), l.valid_until, l.supersedes, COALESCE(l.importance, 3), l.supersede_status, COALESCE(l.noise_count, 0), COALESCE(l.fail_count, 0),
		COALESCE(l.match_count, 0), COALESCE(l.inject_count, 0), COALESCE(l.use_count, 0), COALESCE(l.save_count, 0), COALESCE(l.stability, 30.0),
		COALESCE(l.context, ''), COALESCE(l.domain, 'code'), COALESCE(l.trigger_rule, ''), COALESCE(l.embedding_text, ''),
		COALESCE(l.source_file, ''), COALESCE(l.source_hash, ''), COALESCE(l.doc_chunk_ref, 0), COALESCE(l.task_type, ''), COALESCE(l.turns_at_creation, 0), COALESCE(l.origin_tool, ''), COALESCE(l.source_msg_from, -1), COALESCE(l.source_msg_to, -1),
		COALESCE(l.canonical_project, ''),
		COALESCE(l.attribution, ''),
		COALESCE(l.staleness_score, 0.0), COALESCE(l.staleness_reason, ''), l.staleness_checked_at, COALESCE(l.staleness_type, ''), COALESCE(l.code_fingerprint, ''), COALESCE(l.project_source, '')
		FROM learnings l
		JOIN learning_entities le ON le.learning_id = l.id
		WHERE le.value IN (` + strings.Join(placeholders, ",") + `)
		AND l.superseded_by IS NULL
		AND (l.valid_until IS NULL OR julianday(l.valid_until) > julianday('now'))`

	if project != "" {
		query += ` AND (l.canonical_project = ? OR l.canonical_project = '')`
		args = append(args, project)
	}

	query += ` ORDER BY l.created_at DESC LIMIT 20`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanLearnings(rows)
}

// SearchAnticipatedQueries searches the anticipated_queries_fts table (porter stemming).
// Returns parent learning content for matching AQs, deduplicated by learning_id.
func (s *Store) SearchAnticipatedQueries(query, project string, limit int, since, before string) ([]LearningSearchResult, error) {
	words := splitWords(query)
	if len(words) < 2 {
		return nil, nil
	}
	if err := validateFilterBounds(since, before); err != nil {
		return nil, err
	}
	terms := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.ReplaceAll(w, `"`, "")
		if w == "" {
			continue
		}
		terms = append(terms, `"`+w+`"`)
	}
	for len(terms) >= 2 {
		ftsQuery := strings.Join(terms, " AND ")
		results, err := s.runAQFTSQuery(ftsQuery, project, limit, since, before)
		if err != nil {
			return nil, err
		}
		if len(results) > 0 {
			return results, nil
		}
		terms = terms[:len(terms)-1]
	}
	return nil, nil
}

func (s *Store) runAQFTSQuery(ftsQuery, project string, limit int, since, before string) ([]LearningSearchResult, error) {
	where := `MATCH ? AND ` + models.RecallableLearningSQLAlias
	args := []any{ftsQuery}
	// julianday() on the column makes the mixed offsets in created_at comparable;
	// the bound is normalized so its spelling never matters.
	if since != "" {
		where += ` AND julianday(l.created_at) >= julianday(?)`
		args = append(args, SQLiteFilterTime(since))
	}
	if before != "" {
		where += ` AND julianday(l.created_at) < julianday(?)`
		args = append(args, SQLiteFilterTime(before))
	}

	rows, err := s.readerDB().Query(`
		SELECT aq.learning_id, l.content, bm25(anticipated_queries_fts) AS score, COALESCE(l.project, ''), COALESCE(l.canonical_project, '')
		FROM anticipated_queries_fts aq
		JOIN learnings l ON l.id = aq.learning_id
		WHERE anticipated_queries_fts `+where+`
		ORDER BY bm25(anticipated_queries_fts)
		LIMIT ?`, append(args, limit*3)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[int64]bool)
	var results []LearningSearchResult
	for rows.Next() {
		var lid int64
		var content string
		var score float64
		var proj, canon string
		if err := rows.Scan(&lid, &content, &score, &proj, &canon); err != nil {
			continue
		}
		if project != "" && !projectMatchesTolerant(proj, canon, project) {
			continue
		}
		if seen[lid] {
			continue
		}
		seen[lid] = true
		results = append(results, LearningSearchResult{
			ID:      fmt.Sprintf("%d", lid),
			Content: content,
			Score:   -score,
			Project: proj,
		})
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

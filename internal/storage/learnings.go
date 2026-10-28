package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// resolveCanonicalProject resolves a project name to its canonical form for queries.
// 1. Full paths with .worktrees/ resolve via models.CanonicalProject directly.
// 2. Short names resolve via DB lookup: what canonical_project do existing learnings use?
func (s *Store) resolveCanonicalProject(project string) string {
	if project == "" {
		return project
	}
	canonical := models.CanonicalProject(project)
	if canonical != project && canonical != filepath.Base(project) {
		return canonical
	}
	var dbCanonical string
	err := s.readerDB().QueryRow(
		`SELECT canonical_project FROM learnings WHERE project = ? AND canonical_project != ? AND canonical_project != '' LIMIT 1`,
		project, project,
	).Scan(&dbCanonical)
	if err == nil && dbCanonical != "" {
		return dbCanonical
	}
	return project
}

// projectReadRoot returns the directory a project id is scoped by: the path itself,
// or, for a path inside a worktree checkout, the repository root before the marker.
// It deliberately avoids models.RepositoryRoot, which resolves the enclosing
// checkout (YM-321): folding is only ever wanted for the worktree copies of one
// project, and ascending to the checkout would merge sibling projects of that
// checkout with each other.
func projectReadRoot(project string) string {
	cut := len(project)
	for _, marker := range []string{"/.worktrees/", "/.claude/worktrees/"} {
		if i := strings.Index(project, marker); i >= 0 && i < cut {
			cut = i
		}
	}
	root := filepath.Clean(project[:cut])
	if root == string(filepath.Separator) {
		return ""
	}
	return root
}

// ActiveLearningsForProject returns the active learnings a briefing for project
// should see. For a repository checkout it reads the rows at or below the project
// path instead of the canonical basename: a repository whose work happens in
// worktree checkouts has no row under its own path, and the basename scope answers
// with every repository carrying the same folder name. The path scope needs no
// decision before the read, so a concurrent insert cannot change what it matches.
//
// A worktree copy keeps the canonical scope (it folds to its repository), and a
// directory that merely contains projects keeps it too: such a directory describes
// none of them and must not answer with theirs. One limit is accepted on purpose:
// rows that still carry a legacy short name reach every repository of that name
// (167 active rows with a relative project and a set canonical on the live DB,
// among them leftovers of a foreign project).
func (s *Store) ActiveLearningsForProject(project, category string, limit int) ([]models.Learning, error) {
	if !filepath.IsAbs(project) {
		return s.GetActiveLearnings(category, project, "", "", limit)
	}
	clean := filepath.Clean(project)
	if markerRoot := projectReadRoot(clean); markerRoot != "" && markerRoot != clean {
		return s.GetActiveLearnings(category, project, "", "", limit)
	}
	if !isRepositoryRoot(clean) {
		return s.GetActiveLearnings(category, project, "", "", limit)
	}
	return s.GetActiveLearningsFiltered(LearningsFilter{Category: category, ProjectSubtree: clean}, limit)
}

// InsertLearning inserts a learning and returns its ID.
func (s *Store) InsertLearning(l *models.Learning) (int64, error) {
	ids, err := s.InsertLearningBatch([]*models.Learning{l})
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, fmt.Errorf("insert learning: no id returned")
	}
	return ids[0], nil
}

// HasPulseForSession checks if a pulse learning already exists for the given session.
func (s *Store) HasPulseForSession(sessionID string) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM learnings WHERE session_id = ? AND category = 'pulse'", sessionID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check pulse for %s: %w", sessionID, err)
	}
	return count > 0, nil
}

// GetPulseLearningsSince returns pulse learnings for a project created after since.
func (s *Store) GetPulseLearningsSince(project string, since time.Time, limit int) ([]models.Learning, error) {
	canonical := s.resolveCanonicalProject(project)
	rows, err := s.readerDB().Query(`SELECT id, session_id, content, created_at FROM learnings
		WHERE category = 'pulse' AND canonical_project = ?
		AND julianday(created_at) > julianday(?) AND `+models.RecallableLearningSQL+`
		ORDER BY created_at ASC LIMIT ?`,
		canonical, SQLiteFilterTime(fmtTime(since)), limit)
	if err != nil {
		return nil, fmt.Errorf("get pulse learnings: %w", err)
	}
	defer rows.Close()

	var result []models.Learning
	for rows.Next() {
		var l models.Learning
		var createdAt string
		if err := rows.Scan(&l.ID, &l.SessionID, &l.Content, &createdAt); err != nil {
			return nil, fmt.Errorf("scan pulse: %w", err)
		}
		l.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		l.Category = "pulse"
		result = append(result, l)
	}
	return result, nil
}

// InsertLearningBatch inserts multiple learnings in a single transaction.
// Returns the IDs of all inserted learnings. One COMMIT instead of N reduces
// FTS5 write contention that blocks concurrent BM25 reads.
func (s *Store) InsertLearningBatch(learnings []*models.Learning) ([]int64, error) {
	if len(learnings) == 0 {
		return nil, nil
	}

	// Normalize learnings whose Project is still a legacy short name: resolve
	// to the session's full project path so post-migration queries match.
	sessionProjectCache := make(map[string]string)
	for _, l := range learnings {
		if l.Project != "" && l.Project[0] != '/' && l.SessionID != "" {
			if full, ok := sessionProjectCache[l.SessionID]; ok {
				l.Project = full
			} else if sess, err := s.GetSession(l.SessionID); err == nil && sess != nil && sess.Project != "" {
				full := models.ProjectShortFromPath(sess.Project)
				sessionProjectCache[l.SessionID] = full
				l.Project = full
			}
		}
	}

	// Bulk-fetch current turn counts BEFORE opening transaction (avoids deadlock with MaxOpenConns=1)
	projects := make(map[string]bool)
	hasGlobal := false
	for _, l := range learnings {
		if l.Project != "" {
			projects[l.Project] = true
		} else {
			hasGlobal = true
		}
	}
	projectList := make([]string, 0, len(projects)+1)
	for p := range projects {
		projectList = append(projectList, p)
	}
	if hasGlobal {
		projectList = append(projectList, "__global__")
	}
	turnCounts, _ := s.GetTurnCountsBulk(projectList)

	// Provenance (YM-74): agent_role and dialog_id exist as columns, conflict
	// detection and the wiki read them, and no path ever wrote them. Both are
	// resolved once per session, before the transaction opens, from data the store
	// already holds.
	provenanceCache := make(map[string]provenance)
	for _, l := range learnings {
		if l.AgentRole != "" && l.DialogID != 0 {
			continue
		}
		p, ok := provenanceCache[l.SessionID]
		if !ok {
			p = s.sessionProvenance(l.SessionID)
			provenanceCache[l.SessionID] = p
		}
		if l.AgentRole == "" {
			l.AgentRole = p.agentRole
		}
		if l.DialogID == 0 {
			l.DialogID = p.dialogID
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO learnings
		(session_id, category, content, project, confidence, created_at, expires_at, valid_until, model_used, source, emotional_intensity, session_flavor, supersedes, importance, hit_count, fail_count, context, domain, trigger_rule, embedding_text, embedding_status, embedding_content_hash, embedded_at, source_file, source_hash, doc_chunk_ref, content_hash, task_type, turns_at_creation, source_msg_from, source_msg_to, origin_tool, source_agent, target_agent, canonical_project, attribution, project_source, agent_role, dialog_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	// Keep learnings_fts read-after-write consistent for interactive search/resolve
	// paths without relying on the periodic background sync.
	ftsStmt, err := tx.Prepare(`INSERT OR REPLACE INTO learnings_fts(rowid, content) VALUES (?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare learnings fts: %w", err)
	}
	defer ftsStmt.Close()

	var ids []int64
	for _, l := range learnings {
		var expiresAt *string
		if l.ExpiresAt != nil {
			t := fmtTime(*l.ExpiresAt)
			expiresAt = &t
		}
		var validUntil *string
		if l.ValidUntil != nil {
			t := fmtTime(*l.ValidUntil)
			validUntil = &t
		}

		source := l.Source
		if source == "" {
			source = "llm_extracted"
		}
		if l.ContentHash == "" {
			l.ContentHash = textutil.ContentHash(l.Content)
		}
		embeddingStatus, embeddingContentHash := prepareEmbeddingTracking(l)

		importance := l.Importance
		if importance == 0 {
			switch source {
			case "user_stated", "agreed_upon":
				importance = 4
			case "claude_suggested":
				importance = 2
			default:
				importance = 3
			}
		}

		// A zero CreatedAt used to reach the column as "" (fmtTime renders the zero
		// time as the empty string), and every julianday() window drops such a row.
		// Extraction passes the session start and leaves it zero when that start
		// never parsed, so the insertion instant is the honest fallback.
		if l.CreatedAt.IsZero() {
			l.CreatedAt = time.Now()
		}

		turnKey := l.Project
		if turnKey == "" {
			turnKey = "__global__"
		}

		if l.CanonicalProject == "" && l.Project != "" {
			l.CanonicalProject = models.CanonicalProject(l.Project)
		}

		// Resolve source/target agent: use explicit value from learning, fall back to session derivation.
		srcAgent := models.NormalizeSourceAgent(l.SourceAgent)
		tgtAgent := l.TargetAgent
		if srcAgent == "" && l.SessionID != "" {
			srcAgent = s.sessionSourceAgent(l.SessionID)
		}

		// Lineage sentinel: Go zero-value (0) means "not set" — normalize to -1
		// so we can distinguish "no lineage" from "extracted from message 0".
		sourceMsgFrom, sourceMsgTo := l.SourceMsgFrom, l.SourceMsgTo
		if sourceMsgFrom == 0 && sourceMsgTo == 0 {
			sourceMsgFrom, sourceMsgTo = -1, -1
		}

		result, err := stmt.Exec(
			l.SessionID, l.Category, l.Content, l.Project, l.Confidence,
			fmtTime(l.CreatedAt), expiresAt, validUntil, l.ModelUsed, source, l.EmotionalIntensity, l.SessionFlavor, l.Supersedes, importance, l.HitCount, l.FailCount,
			l.Context, l.Domain, l.TriggerRule, l.EmbeddingText, embeddingStatus, embeddingContentHash, l.SourceFile, l.SourceHash, l.DocChunkRef, l.ContentHash, l.TaskType, turnCounts[turnKey], sourceMsgFrom, sourceMsgTo, l.OriginTool, srcAgent, tgtAgent, l.CanonicalProject, l.Attribution, l.ProjectSource, l.AgentRole, l.DialogID)
		if err != nil {
			return ids, fmt.Errorf("insert learning: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)

		searchableText := BuildSearchableText(l.Content, l.Keywords, l.AnticipatedQueries, l.Entities)
		if _, err := ftsStmt.Exec(id, searchableText); err != nil {
			return ids, fmt.Errorf("insert learning fts: %w", err)
		}

		// Junction tables within same transaction
		for _, e := range l.Entities {
			tx.Exec("INSERT INTO learning_entities (learning_id, value) VALUES (?, ?)", id, e)
		}
		for _, a := range l.Actions {
			tx.Exec("INSERT INTO learning_actions (learning_id, value) VALUES (?, ?)", id, a)
		}
		for _, k := range l.Keywords {
			tx.Exec("INSERT INTO learning_keywords (learning_id, value) VALUES (?, ?)", id, k)
		}
		for _, q := range l.AnticipatedQueries {
			tx.Exec("INSERT INTO learning_anticipated_queries (learning_id, value) VALUES (?, ?)", id, q)
			tx.Exec("INSERT INTO anticipated_queries_fts (value, learning_id) VALUES (?, ?)", q, id)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit batch: %w", err)
	}
	return ids, nil
}

// RebuildFTSEnriched rebuilds the FTS index with enriched content (content + keywords + anticipated_queries + entities).
// Safe alternative to trigger-based enrichment — avoids FTS5 delete bug with modernc/sqlite [ID:37853].
// Call once after batch insertion, not per-learning.
func (s *Store) RebuildFTSEnriched() (int, error) {
	rows, err := s.db.Query(`SELECT l.id, l.content FROM learnings l WHERE l.superseded_by IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("query learnings: %w", err)
	}
	defer rows.Close()

	type entry struct {
		id      int64
		content string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.content); err != nil {
			continue
		}
		entries = append(entries, e)
	}

	// Load ALL junction data BEFORE starting transaction (avoids deadlock with MaxOpenConns=1)
	type enrichedEntry struct {
		id       int64
		enriched string
	}
	var enriched []enrichedEntry
	for _, e := range entries {
		var keywords, queries, entities []string
		kRows, _ := s.readerDB().Query("SELECT value FROM learning_keywords WHERE learning_id = ?", e.id)
		if kRows != nil {
			for kRows.Next() {
				var v string
				kRows.Scan(&v)
				keywords = append(keywords, v)
			}
			kRows.Close()
		}
		qRows, _ := s.readerDB().Query("SELECT value FROM learning_anticipated_queries WHERE learning_id = ?", e.id)
		if qRows != nil {
			for qRows.Next() {
				var v string
				qRows.Scan(&v)
				queries = append(queries, v)
			}
			qRows.Close()
		}
		eRows, _ := s.readerDB().Query("SELECT value FROM learning_entities WHERE learning_id = ?", e.id)
		if eRows != nil {
			for eRows.Next() {
				var v string
				eRows.Scan(&v)
				entities = append(entities, v)
			}
			eRows.Close()
		}
		enriched = append(enriched, enrichedEntry{id: e.id, enriched: BuildSearchableText(e.content, keywords, queries, entities)})
	}

	// Rebuild FTS: clear + populate with enriched content (all reads done, only writes below)
	if _, err := s.db.Exec(`DELETE FROM learnings_fts`); err != nil {
		return 0, fmt.Errorf("clear fts: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT INTO learnings_fts(rowid, content) VALUES (?, ?)`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	count := 0
	for _, e := range enriched {
		if _, err := stmt.Exec(e.id, e.enriched); err != nil {
			log.Printf("  [fts] skip %d: %v", e.id, err)
			continue
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit fts rebuild: %w", err)
	}
	return count, nil
}

// BuildSearchableText combines content with keywords, anticipated_queries, entities, and attribution.
// This makes metadata searchable via BM25 without changing the stored content.
func BuildSearchableText(content string, keywords, anticipatedQueries, entities []string) string {
	if len(keywords) == 0 && len(anticipatedQueries) == 0 && len(entities) == 0 {
		return content
	}
	var sb strings.Builder
	sb.WriteString(content)
	for _, v := range keywords {
		sb.WriteString(" ")
		sb.WriteString(v)
	}
	for _, v := range anticipatedQueries {
		sb.WriteString(" ")
		sb.WriteString(v)
	}
	for _, v := range entities {
		sb.WriteString(" ")
		sb.WriteString(v)
	}
	return sb.String()
}

// GetLearningsByCategory returns active learnings of a specific category.
func (s *Store) GetLearningsByCategory(category, project string, limit int) ([]models.Learning, error) {
	canonical := s.resolveCanonicalProject(project)
	query := `SELECT id, session_id, category, content, project, confidence,
		superseded_by, supersede_reason, created_at, expires_at, model_used, source,
		COALESCE(hit_count, 0), COALESCE(emotional_intensity, 0.0), last_hit_at, COALESCE(session_flavor, ''), valid_until, supersedes, COALESCE(importance, 3), supersede_status, COALESCE(noise_count, 0), COALESCE(fail_count, 0),
		COALESCE(match_count, 0), COALESCE(inject_count, 0), COALESCE(use_count, 0), COALESCE(save_count, 0), COALESCE(stability, 30.0),
		COALESCE(context, ''), COALESCE(domain, 'code'), COALESCE(trigger_rule, ''), COALESCE(embedding_text, ''),
		COALESCE(source_file, ''), COALESCE(source_hash, ''), COALESCE(doc_chunk_ref, 0), COALESCE(task_type, ''), COALESCE(turns_at_creation, 0), COALESCE(origin_tool, ''), COALESCE(source_msg_from, -1), COALESCE(source_msg_to, -1),
		COALESCE(canonical_project, ''),
		COALESCE(attribution, ''),
		COALESCE(staleness_score, 0.0), COALESCE(staleness_reason, ''), staleness_checked_at, COALESCE(staleness_type, ''), COALESCE(code_fingerprint, ''), COALESCE(project_source, '')
		FROM learnings WHERE superseded_by IS NULL
		AND category = ?
		AND (canonical_project = ? OR canonical_project = '')
		ORDER BY created_at DESC
		LIMIT ?`
	rows, err := s.readerDB().Query(query, category, canonical, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLearnings(rows)
}

// GetLearningsSince returns active learnings created after the given time.
// Filters to decision, pattern, gotcha, pivot_moment categories (most useful for metamemory).
func (s *Store) GetLearningsSince(project string, since time.Time, limit int) ([]models.Learning, error) {
	query := `SELECT id, session_id, category, content, project, confidence,
		superseded_by, supersede_reason, created_at, expires_at, model_used, source,
		COALESCE(hit_count, 0), COALESCE(emotional_intensity, 0.0), last_hit_at, COALESCE(session_flavor, ''), valid_until, supersedes, COALESCE(importance, 3), supersede_status, COALESCE(noise_count, 0), COALESCE(fail_count, 0),
		COALESCE(match_count, 0), COALESCE(inject_count, 0), COALESCE(use_count, 0), COALESCE(save_count, 0), COALESCE(stability, 30.0),
		COALESCE(context, ''), COALESCE(domain, 'code'), COALESCE(trigger_rule, ''), COALESCE(embedding_text, ''),
		COALESCE(source_file, ''), COALESCE(source_hash, ''), COALESCE(doc_chunk_ref, 0), COALESCE(task_type, ''), COALESCE(turns_at_creation, 0), COALESCE(origin_tool, ''), COALESCE(source_msg_from, -1), COALESCE(source_msg_to, -1),
		COALESCE(canonical_project, ''),
		COALESCE(attribution, ''),
		COALESCE(staleness_score, 0.0), COALESCE(staleness_reason, ''), staleness_checked_at, COALESCE(staleness_type, ''), COALESCE(code_fingerprint, ''), COALESCE(project_source, '')
		FROM learnings WHERE ` + models.RecallableLearningSQL + `
		AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))
		AND julianday(created_at) > julianday(?)
		AND category IN ('decision', 'pattern', 'gotcha', 'pivot_moment', 'unfinished', 'explicit_teaching')
		AND canonical_project = ?
		ORDER BY created_at DESC
		LIMIT ?`

	canonical := s.resolveCanonicalProject(project)
	rows, err := s.readerDB().Query(query, fmtTime(time.Now()), SQLiteFilterTime(fmtTime(since)), canonical, limit)
	if err != nil {
		return nil, fmt.Errorf("get learnings since: %w", err)
	}
	defer rows.Close()
	return scanLearnings(rows)
}

// GetSessionFlavorsSince returns distinct session flavors grouped by session_id since the given time.
func (s *Store) GetSessionFlavorsSince(project string, since time.Time, limit int) ([]map[string]any, error) {
	canonical := s.resolveCanonicalProject(project)
	query := `SELECT session_flavor, MIN(created_at) as first_seen, session_id
		FROM learnings
		WHERE ` + models.RecallableLearningSQL + `
		AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))
		AND julianday(created_at) > julianday(?)
		AND length(session_flavor) > 0
		AND canonical_project = ?
		GROUP BY session_id
		ORDER BY first_seen ASC
		LIMIT ?`

	rows, err := s.readerDB().Query(query, fmtTime(time.Now()), SQLiteFilterTime(fmtTime(since)), canonical, limit)
	if err != nil {
		return nil, fmt.Errorf("get session flavors since: %w", err)
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var flavor, sessionID, createdAt string
		if err := rows.Scan(&flavor, &createdAt, &sessionID); err != nil {
			continue
		}
		results = append(results, map[string]any{
			"session_flavor": flavor,
			"created_at":     createdAt,
			"session_id":     sessionID,
		})
	}
	return results, nil
}

// GetAgentRolesBulk returns agent_role for a list of learning IDs.
func (s *Store) GetAgentRolesBulk(ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf("SELECT id, COALESCE(agent_role, '') FROM learnings WHERE id IN (%s)", strings.Join(placeholders, ","))
	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]string, len(ids))
	for rows.Next() {
		var id int64
		var role string
		if rows.Scan(&id, &role) == nil && role != "" {
			result[fmt.Sprintf("%d", id)] = role
		}
	}
	return result, nil
}

// GetActiveCategories returns all distinct categories that have active (non-superseded) learnings.
func (s *Store) GetActiveCategories() ([]string, error) {
	rows, err := s.readerDB().Query(`SELECT DISTINCT category FROM learnings
		WHERE superseded_by IS NULL ORDER BY category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cats []string
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			cats = append(cats, c)
		}
	}
	return cats, rows.Err()
}

// GetMaxLearningID returns the current insertion high-watermark.
func (s *Store) GetMaxLearningID() (int64, error) {
	var id int64
	if err := s.readerDB().QueryRow(`SELECT COALESCE(MAX(id), 0) FROM learnings`).Scan(&id); err != nil {
		return 0, fmt.Errorf("get max learning id: %w", err)
	}
	return id, nil
}

// GetConsolidationLearnings returns active learnings from only the canonical
// project/category scopes touched in (afterID, throughID]. Existing learnings
// in those scopes are included as comparison candidates.
func (s *Store) GetConsolidationLearnings(afterID, throughID int64) ([]models.Learning, error) {
	now := fmtTime(time.Now())
	rows, err := s.readerDB().Query(`WITH dirty(category, project) AS (
		SELECT DISTINCT category, COALESCE(NULLIF(canonical_project, ''), COALESCE(project, ''))
		FROM learnings
		WHERE id > ? AND id <= ? AND superseded_by IS NULL
		AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))
		AND category NOT IN ('narrative', 'cap')
	)
	SELECT l.id, l.session_id, l.category, l.content, l.project, l.confidence,
		l.superseded_by, l.supersede_reason, l.created_at, l.expires_at, l.model_used, l.source,
		COALESCE(l.hit_count, 0), COALESCE(l.emotional_intensity, 0.0), l.last_hit_at, COALESCE(l.session_flavor, ''), l.valid_until, l.supersedes, COALESCE(l.importance, 3), l.supersede_status, COALESCE(l.noise_count, 0), COALESCE(l.fail_count, 0),
		COALESCE(l.match_count, 0), COALESCE(l.inject_count, 0), COALESCE(l.use_count, 0), COALESCE(l.save_count, 0), COALESCE(l.stability, 30.0),
		COALESCE(l.context, ''), COALESCE(l.domain, 'code'), COALESCE(l.trigger_rule, ''), COALESCE(l.embedding_text, ''),
		COALESCE(l.source_file, ''), COALESCE(l.source_hash, ''), COALESCE(l.doc_chunk_ref, 0), COALESCE(l.task_type, ''), COALESCE(l.turns_at_creation, 0), COALESCE(l.origin_tool, ''), COALESCE(l.source_msg_from, -1), COALESCE(l.source_msg_to, -1),
		COALESCE(l.canonical_project, ''),
		COALESCE(l.attribution, ''),
		COALESCE(l.staleness_score, 0.0), COALESCE(l.staleness_reason, ''), l.staleness_checked_at, COALESCE(l.staleness_type, ''), COALESCE(l.code_fingerprint, ''), COALESCE(l.project_source, '')
	FROM learnings l
	JOIN dirty d ON d.category = l.category
		AND d.project = COALESCE(NULLIF(l.canonical_project, ''), COALESCE(l.project, ''))
	WHERE l.id <= ? AND ` + models.RecallableLearningSQLAlias + `
	AND (l.expires_at IS NULL OR julianday(l.expires_at) > julianday(?))
	ORDER BY l.category, l.canonical_project, l.project, l.id DESC`, afterID, throughID, now, throughID, now)
	if err != nil {
		return nil, fmt.Errorf("get consolidation learnings: %w", err)
	}
	defer rows.Close()
	return scanLearnings(rows)
}

// LearningsFilter narrows a GetActiveLearningsFiltered query. Empty fields do
// not filter.
type LearningsFilter struct {
	Category string
	Project  string
	Since    string
	Before   string
	TaskType string
	// ProjectSubtree selects the rows at or below this absolute path instead of the
	// canonical scope of Project, which it replaces when both are set. It is the
	// path-scoped read for a project whose work lives in checkouts below its own
	// path.
	ProjectSubtree string
}

// GetActiveLearnings returns all non-superseded, non-expired learnings.
// Filter by category and/or project (empty string = no filter).
func (s *Store) GetActiveLearnings(category, project, since, before string, limit int) ([]models.Learning, error) {
	return s.GetActiveLearningsFiltered(LearningsFilter{
		Category: category, Project: project, Since: since, Before: before,
	}, limit)
}

// GetActiveLearningsFiltered is GetActiveLearnings with every filter applied in
// the query. A task_type filter has to run here: applied to an already capped
// result it silently drops rows that the window did not contain.
func (s *Store) GetActiveLearningsFiltered(f LearningsFilter, limit int) ([]models.Learning, error) {
	category, project, since, before, taskType := f.Category, f.Project, f.Since, f.Before, f.TaskType
	subtree := f.ProjectSubtree
	if err := validateFilterBounds(since, before); err != nil {
		return nil, err
	}
	query := `SELECT id, session_id, category, content, project, confidence,
		superseded_by, supersede_reason, created_at, expires_at, model_used, source,
		COALESCE(hit_count, 0), COALESCE(emotional_intensity, 0.0), last_hit_at, COALESCE(session_flavor, ''), valid_until, supersedes, COALESCE(importance, 3), supersede_status, COALESCE(noise_count, 0), COALESCE(fail_count, 0),
		COALESCE(match_count, 0), COALESCE(inject_count, 0), COALESCE(use_count, 0), COALESCE(save_count, 0), COALESCE(stability, 30.0),
		COALESCE(context, ''), COALESCE(domain, 'code'), COALESCE(trigger_rule, ''), COALESCE(embedding_text, ''),
		COALESCE(source_file, ''), COALESCE(source_hash, ''), COALESCE(doc_chunk_ref, 0), COALESCE(task_type, ''), COALESCE(turns_at_creation, 0), COALESCE(origin_tool, ''), COALESCE(source_msg_from, -1), COALESCE(source_msg_to, -1),
		COALESCE(canonical_project, ''),
		COALESCE(attribution, ''),
		COALESCE(staleness_score, 0.0), COALESCE(staleness_reason, ''), staleness_checked_at, COALESCE(staleness_type, ''), COALESCE(code_fingerprint, ''), COALESCE(project_source, '')
		FROM learnings WHERE ` + models.RecallableLearningSQL + `
		AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))`
	args := []any{fmtTime(time.Now())}

	if category != "" {
		query += ` AND category = ?`
		args = append(args, category)
	}
	if subtree != "" {
		prefix := subtree + string(filepath.Separator)
		hi := prefix[:len(prefix)-1] + string(byte(filepath.Separator)+1)
		query += ` AND (canonical_project = '' OR project = ? OR (project >= ? AND project < ?) OR (project NOT LIKE '/%' AND canonical_project = ?))`
		args = append(args, subtree, prefix, hi, models.CanonicalProject(subtree))
	} else if project != "" {
		canonical := s.resolveCanonicalProject(project)
		query += ` AND (canonical_project = ? OR canonical_project = '')`
		args = append(args, canonical)
	}
	// Compare parsed instants: created_at is RFC3339 in the writer's zone, so a
	// lexical compare against a UTC or date-only bound is off by the offset. The
	// column-side julianday() is what makes mixed offsets comparable; it costs the
	// created_at index, which only this since/before path paid for anyway.
	if since != "" {
		query += ` AND julianday(created_at) >= julianday(?)`
		args = append(args, SQLiteFilterTime(since))
	}
	if before != "" {
		query += ` AND julianday(created_at) < julianday(?)`
		args = append(args, SQLiteFilterTime(before))
	}
	if taskType != "" {
		query += ` AND COALESCE(task_type, '') = ?`
		args = append(args, taskType)
	}
	query += ` ORDER BY category, created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get active learnings: %w", err)
	}
	defer rows.Close()

	learnings, err := scanLearnings(rows)
	if err != nil {
		return nil, err
	}
	// Every consumer of this read path scores with models.ComputeScore, which
	// reads CurrentTurnCount. Resolve it here, once, instead of expecting each
	// caller to remember (none did: turn-based decay never ran).
	rows.Close()
	s.EnrichTurnCounts(learnings)
	return learnings, nil
}

// GetActiveLearningsBySessionIDs returns active learnings for the given session IDs.
func (s *Store) GetActiveLearningsBySessionIDs(sessionIDs []string) ([]models.Learning, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}

	// Dedupe: with batching, an ID repeated in two batches would return its rows
	// twice, while a single IN list deduplicated them implicitly.
	unique := make([]string, 0, len(sessionIDs))
	seen := make(map[string]struct{}, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		unique = append(unique, sessionID)
	}

	const batchSize = 500
	now := fmtTime(time.Now())
	var all []models.Learning
	// One placeholder per ID exceeds SQLite's variable limit on a large list and
	// fails the whole read, so the IDs go in batches. The result is ordered per
	// batch, no longer globally by created_at; no caller depends on that order.
	for _, batch := range chunkStrings(unique, batchSize) {
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch)+1)
		args = append(args, now)
		for i, sessionID := range batch {
			placeholders[i] = "?"
			args = append(args, sessionID)
		}

		query := `SELECT id, session_id, category, content, project, confidence,
			superseded_by, supersede_reason, created_at, expires_at, model_used, source,
			COALESCE(hit_count, 0), COALESCE(emotional_intensity, 0.0), last_hit_at, COALESCE(session_flavor, ''), valid_until, supersedes, COALESCE(importance, 3), supersede_status, COALESCE(noise_count, 0), COALESCE(fail_count, 0),
			COALESCE(match_count, 0), COALESCE(inject_count, 0), COALESCE(use_count, 0), COALESCE(save_count, 0), COALESCE(stability, 30.0),
			COALESCE(context, ''), COALESCE(domain, 'code'), COALESCE(trigger_rule, ''), COALESCE(embedding_text, ''),
			COALESCE(source_file, ''), COALESCE(source_hash, ''), COALESCE(doc_chunk_ref, 0), COALESCE(task_type, ''), COALESCE(turns_at_creation, 0), COALESCE(origin_tool, ''), COALESCE(source_msg_from, -1), COALESCE(source_msg_to, -1),
			COALESCE(canonical_project, ''),
			COALESCE(attribution, ''),
			COALESCE(staleness_score, 0.0), COALESCE(staleness_reason, ''), staleness_checked_at, COALESCE(staleness_type, ''), COALESCE(code_fingerprint, ''), COALESCE(project_source, '')
			FROM learnings
			WHERE superseded_by IS NULL
			AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))
			AND session_id IN (` + strings.Join(placeholders, ",") + `)
			ORDER BY created_at DESC`

		rows, err := s.readerDB().Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("get active learnings by session IDs: %w", err)
		}
		learnings, err := scanLearnings(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, learnings...)
	}
	return all, nil
}

// GetLearningsForClaudeMd returns active learnings for a project with per-category caps.
func (s *Store) GetLearningsForClaudeMd(project string, maxPerCategory map[string]int) ([]models.Learning, error) {
	all, err := s.GetActiveLearnings("", project, "", "", 0)
	if err != nil {
		return nil, err
	}
	// Score-sort so per-category cap keeps highest-value learnings
	models.ScoreAndSort(all)
	if len(maxPerCategory) == 0 {
		return all, nil
	}
	counts := map[string]int{}
	result := make([]models.Learning, 0, len(all))
	for _, l := range all {
		cap, ok := maxPerCategory[l.Category]
		if !ok {
			cap = 10
		}
		if counts[l.Category] < cap {
			result = append(result, l)
			counts[l.Category]++
		}
	}
	return result, nil
}

// GetLearning returns a single learning by ID.
func (s *Store) GetLearning(id int64) (*models.Learning, error) {
	row := s.readerDB().QueryRow(`SELECT id, session_id, category, content, project, confidence,
		superseded_by, supersede_reason, created_at, expires_at, model_used, source,
		COALESCE(hit_count, 0), COALESCE(emotional_intensity, 0.0), last_hit_at, COALESCE(session_flavor, ''), valid_until, supersedes, COALESCE(importance, 3), supersede_status, COALESCE(noise_count, 0), COALESCE(fail_count, 0),
		COALESCE(match_count, 0), COALESCE(inject_count, 0), COALESCE(use_count, 0), COALESCE(save_count, 0), COALESCE(stability, 30.0),
		COALESCE(context, ''), COALESCE(domain, 'code'), COALESCE(trigger_rule, ''), COALESCE(embedding_text, ''),
		COALESCE(source_file, ''), COALESCE(source_hash, ''), COALESCE(doc_chunk_ref, 0), COALESCE(task_type, ''), COALESCE(turns_at_creation, 0), COALESCE(origin_tool, ''), COALESCE(source_msg_from, -1), COALESCE(source_msg_to, -1),
		COALESCE(canonical_project, ''),
		COALESCE(attribution, ''),
		COALESCE(staleness_score, 0.0), COALESCE(staleness_reason, ''), COALESCE(staleness_checked_at, ''), COALESCE(staleness_type, ''), COALESCE(code_fingerprint, ''), COALESCE(project_source, '')
		FROM learnings WHERE id = ?`, id)

	l := &models.Learning{}
	var createdAt string
	var expiresAt, sessionID, project, supersedeReason, source, lastHitAt, validUntil, supersedeStatus *string
	var supersededBy, supersedes *int64
	var projectSource string
	err := row.Scan(&l.ID, &sessionID, &l.Category, &l.Content, &project, &l.Confidence,
		&supersededBy, &supersedeReason, &createdAt, &expiresAt, &l.ModelUsed, &source, &l.HitCount, &l.EmotionalIntensity, &lastHitAt, &l.SessionFlavor, &validUntil, &supersedes, &l.Importance, &supersedeStatus, &l.NoiseCount, &l.FailCount,
		&l.MatchCount, &l.InjectCount, &l.UseCount, &l.SaveCount, &l.Stability,
		&l.Context, &l.Domain, &l.TriggerRule, &l.EmbeddingText,
		&l.SourceFile, &l.SourceHash, &l.DocChunkRef, &l.TaskType, &l.TurnsAtCreation, &l.OriginTool, &l.SourceMsgFrom, &l.SourceMsgTo,
		&l.CanonicalProject, &l.Attribution,
		&l.StalenessScore, &l.StalenessReason, &l.StalenessCheckedAt, &l.StalenessType, &l.CodeFingerprint, &projectSource)
	if err != nil {
		return nil, fmt.Errorf("get learning %d: %w", id, err)
	}
	l.ProjectSource = projectSource
	l.CreatedAt = parseTime(createdAt)
	if source != nil {
		l.Source = *source
	}
	if expiresAt != nil {
		t := parseTime(*expiresAt)
		l.ExpiresAt = &t
	}
	if lastHitAt != nil {
		t := parseTime(*lastHitAt)
		l.LastHitAt = &t
	}
	if sessionID != nil {
		l.SessionID = *sessionID
	}
	if project != nil {
		l.Project = *project
	}
	if supersededBy != nil {
		l.SupersededBy = supersededBy
	}
	if supersedeReason != nil {
		l.SupersedeReason = *supersedeReason
	}
	if validUntil != nil {
		t := parseTime(*validUntil)
		l.ValidUntil = &t
	}
	if supersedes != nil {
		l.Supersedes = supersedes
	}
	if supersedeStatus != nil {
		l.SupersedeStatus = *supersedeStatus
	}
	return l, nil
}

// GetLearningChain returns the full version history of a learning by traversing
// the supersedes chain in both directions. The result is ordered oldest-first.
// Rows that one successor retired together (a batch supersede) are placed
// directly before that successor, in ID order.
//
// A non-positive superseded_by value is a sentinel, not a learning ID: -1 junk
// cleanup, -2 resolved, -3 re-extract, -4 successor deleted, and 0 for the
// rule-based substance-free retire. The row carrying it is the last member of
// the chain; the walk ends there instead of following it to a row that does not
// exist.
//
// Both loops carry a visited-set: if a row references itself (supersedes=id or
// superseded_by=id) or forms a longer cycle, traversal terminates instead of
// spinning forever. This is defense-in-depth against malformed rows; the
// migration in schema.go nulls self-cycles on startup and SupersedeLearningBatch
// rejects self-loops at write time.
//
// A positive edge whose target row no longer exists is a boundary as well. Hard
// deletes used to remove rows without clearing the references that point at
// them, so the walk ends at that row instead of failing the whole history. Only
// the start id itself must exist.
func (s *Store) GetLearningChain(id int64) ([]models.Learning, error) {
	visited := map[int64]bool{id: true}

	// Walk backward from id to root, collecting all ancestors.
	// supersedes points to older version.
	backIDs := []int64{id}
	cur := id
	for {
		var supersedes *int64
		err := s.readerDB().QueryRow(`SELECT supersedes FROM learnings WHERE id=?`, cur).Scan(&supersedes)
		if err != nil {
			if cur != id && errors.Is(err, sql.ErrNoRows) {
				// A hard delete removed the row this edge names without clearing
				// the reference. The walk already collected that id, so it must be
				// dropped again: the target is not a chain member. The branch found
				// so far stays readable and the history ends at the boundary.
				backIDs = backIDs[:len(backIDs)-1]
				break
			}
			return nil, fmt.Errorf("get learning chain: traverse backward from %d: %w", id, err)
		}
		if supersedes == nil || *supersedes <= 0 || *supersedes == cur || visited[*supersedes] {
			// <= 0 mirrors the forward guard: the backward edge is only written
			// with a real ID, so a non-positive value can only be a sentinel and
			// must not be chased either.
			// *supersedes == cur is the explicit self-loop short-circuit;
			// visited[*supersedes] covers longer cycles. Both terminate safely.
			break
		}
		visited[*supersedes] = true
		backIDs = append(backIDs, *supersedes)
		cur = *supersedes
	}
	// Walk forward from id to newest, collecting all descendants.
	// superseded_by points to newer version.
	forwardIDs := []int64{}
	cur = id
	for {
		var supersededBy *int64
		err := s.readerDB().QueryRow(`SELECT superseded_by FROM learnings WHERE id=?`, cur).Scan(&supersededBy)
		if err != nil {
			if cur != id && errors.Is(err, sql.ErrNoRows) {
				// Same boundary as the backward walk: the successor was deleted
				// without clearing the edge. Drop the unresolvable id and keep the
				// branch that was already found.
				forwardIDs = forwardIDs[:len(forwardIDs)-1]
				break
			}
			return nil, fmt.Errorf("get learning chain: traverse forward from %d: %w", id, err)
		}
		if supersededBy == nil || *supersededBy <= 0 || *supersededBy == cur || visited[*supersededBy] {
			// <= 0 stops the walk on a sentinel instead of querying a row that
			// cannot exist (a negative ID is not a learning, and 0 is the
			// substance-free marker). Those rows are still chain members; they
			// simply have no successor.
			// *supersededBy == cur is the explicit self-loop short-circuit;
			// visited[*supersededBy] covers longer cycles. Both terminate safely.
			break
		}
		visited[*supersededBy] = true
		forwardIDs = append(forwardIDs, *supersededBy)
		cur = *supersededBy
	}

	var results []models.Learning
	// backIDs: [id, parent, ..., root] — reverse for oldest-first
	for i := len(backIDs) - 1; i >= 0; i-- {
		l, err := s.GetLearning(backIDs[i])
		if err != nil {
			return nil, fmt.Errorf("get learning chain: get %d: %w", backIDs[i], err)
		}
		results = append(results, *l)
	}
	// forwardIDs: [child, grandchild, ..., leaf] — already oldest-first after root
	for _, fid := range forwardIDs {
		l, err := s.GetLearning(fid)
		if err != nil {
			return nil, fmt.Errorf("get learning chain: get %d: %w", fid, err)
		}
		results = append(results, *l)
	}

	// A batch supersede retires several rows into one successor. Only one of
	// them fits into the scalar backward edge (learnings.supersedes), while each
	// retired row keeps its own forward edge in superseded_by. Walking the
	// scalar edge alone would hide the rest of the batch from the history, so
	// the remaining predecessors are read from the forward column. A retired row
	// can carry a predecessor of its own, so the read repeats for every row it
	// finds; visited bounds the recursion.
	pending := make([]int64, 0, len(backIDs)+len(forwardIDs))
	pending = append(pending, backIDs...)
	pending = append(pending, forwardIDs...)
	var extra []models.Learning
	for len(pending) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(pending)), ",")
		args := make([]any, 0, len(pending))
		for _, m := range pending {
			args = append(args, m)
		}
		rows, err := s.readerDB().Query(
			`SELECT id FROM learnings WHERE superseded_by IN (`+placeholders+`) ORDER BY id`, args...)
		if err != nil {
			return nil, fmt.Errorf("get learning chain: read batch predecessors: %w", err)
		}
		next := []int64{}
		for rows.Next() {
			var pid int64
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				return nil, fmt.Errorf("get learning chain: scan batch predecessor: %w", err)
			}
			if visited[pid] {
				continue
			}
			visited[pid] = true
			next = append(next, pid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get learning chain: read batch predecessors: %w", err)
		}
		for _, pid := range next {
			l, err := s.GetLearning(pid)
			if err != nil {
				return nil, fmt.Errorf("get learning chain: get %d: %w", pid, err)
			}
			extra = append(extra, *l)
		}
		pending = next
	}
	// Each predecessor stays directly before the row that replaced it, in ID
	// order within the group that shares one successor.
	for _, l := range extra {
		pos := len(results)
		if l.SupersededBy != nil {
			successor := *l.SupersededBy
			for i := range results {
				if results[i].SupersededBy != nil && *results[i].SupersededBy == successor && results[i].ID > l.ID {
					pos = i
					break
				}
			}
			if pos == len(results) {
				for i := range results {
					if results[i].ID == successor {
						pos = i
						break
					}
				}
			}
		}
		results = append(results, models.Learning{})
		copy(results[pos+1:], results[pos:])
		results[pos] = l
	}
	return results, nil
}

const (
	SupersededByResolved  int64 = -2
	SupersededByReextract int64 = -3

	// SupersededByDeleted marks a row whose successor was hard-deleted by a
	// delete path. The row stays retired (NULL would put it back into every
	// active query) and its chain ends there.
	SupersededByDeleted int64 = -4

	// SupersededByLegacyCleanup marks a row retired by the one-off,
	// user-approved cleanup batches of July and August 2026. The 182 rows that
	// carry it are a closed historical set: no code in the tree writes this
	// value, and the batches kept their own backups outside the database. It is
	// named so a reader of the data can tell a deliberate batch retirement from
	// a broken edge (YM-445).
	SupersededByLegacyCleanup int64 = -6

	TaskTypeStale = "stale"

	// BatchCleanupReason is the supersede_reason used by the old resolve-stale cleanup.
	// Items with this reason can be reactivated via ReactivateBatchCleanup.
	BatchCleanupReason = "batch-cleanup: stale >30 days"
)

// ResolveLearning marks an unfinished learning as resolved (superseded_by = -2).
// Returns error if learning doesn't exist or is already superseded.
func (s *Store) ResolveLearning(id int64, reason string) error {
	result, err := s.db.Exec(`UPDATE learnings SET superseded_by = ?, supersede_reason = ?, valid_until = datetime('now')
		WHERE id = ? AND superseded_by IS NULL`,
		SupersededByResolved, reason, id)
	if err != nil {
		return fmt.Errorf("resolve learning %d: %w", id, err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("learning %d not found or already superseded", id)
	}
	return nil
}

// ReopenLearning clears a closure so the learning is active again. It handles
// resolutions (superseded_by = SupersededByResolved) and the automated stale
// batch cleanup, which uses the same sentinel; a learning that points at a real
// successor keeps its link.
func (s *Store) ReopenLearning(id int64) error {
	result, err := s.db.Exec(`UPDATE learnings
		SET superseded_by = NULL, supersede_reason = NULL, valid_until = NULL, supersede_status = NULL
		WHERE id = ? AND superseded_by = ?`, id, SupersededByResolved)
	if err != nil {
		return fmt.Errorf("reopen learning %d: %w", id, err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("learning %d is not a resolved item (unknown id, still active, or superseded by another learning)", id)
	}
	return nil
}

// GetTriggeredLearnings returns active learnings whose trigger_rule starts with
// triggerPrefix, is not older than minRule (inclusive, compared lexicographically;
// empty = no lower bound) and that haven't been triggered recently (12h cooldown
// via last_hit_at). Rows are ordered by trigger_rule, so for deadline rules
// ("deadline:YYYY-MM-DD") the earliest still-relevant deadline comes first and is
// not crowded out of the row limit by unrelated or long-expired rules.
func (s *Store) GetTriggeredLearnings(project, triggerPrefix, minRule string) ([]models.Learning, error) {
	canonical := s.resolveCanonicalProject(project)
	now := time.Now()
	cooldown := now.Add(-12 * time.Hour)
	query := `SELECT id, session_id, category, content, project, confidence,
		superseded_by, supersede_reason, created_at, expires_at, model_used, source,
		COALESCE(hit_count, 0), COALESCE(emotional_intensity, 0.0), last_hit_at, COALESCE(session_flavor, ''), valid_until, supersedes, COALESCE(importance, 3), supersede_status, COALESCE(noise_count, 0), COALESCE(fail_count, 0),
		COALESCE(match_count, 0), COALESCE(inject_count, 0), COALESCE(use_count, 0), COALESCE(save_count, 0), COALESCE(stability, 30.0),
		COALESCE(context, ''), COALESCE(domain, 'code'), COALESCE(trigger_rule, ''), COALESCE(embedding_text, ''),
		COALESCE(source_file, ''), COALESCE(source_hash, ''), COALESCE(doc_chunk_ref, 0), COALESCE(task_type, ''), COALESCE(turns_at_creation, 0), COALESCE(origin_tool, ''), COALESCE(source_msg_from, -1), COALESCE(source_msg_to, -1),
		COALESCE(canonical_project, ''),
		COALESCE(attribution, ''),
		COALESCE(staleness_score, 0.0), COALESCE(staleness_reason, ''), staleness_checked_at, COALESCE(staleness_type, ''), COALESCE(code_fingerprint, ''), COALESCE(project_source, '')
		FROM learnings
		WHERE ` + models.RecallableLearningSQL + `
		AND trigger_rule != '' AND trigger_rule IS NOT NULL
		AND trigger_rule LIKE ?
		AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))
		AND (last_hit_at IS NULL OR julianday(last_hit_at) < julianday(?))
		AND (canonical_project = ? OR canonical_project = '')
		AND (? = '' OR trigger_rule >= ?)
		ORDER BY trigger_rule ASC
		LIMIT 10`
	rows, err := s.readerDB().Query(query, triggerPrefix+"%", fmtTime(now), fmtTime(cooldown), canonical, minRule, minRule)
	if err != nil {
		return nil, fmt.Errorf("get triggered learnings: %w", err)
	}
	defer rows.Close()
	return scanLearnings(rows)
}

// splitWords splits a string into lowercase words.
func splitWords(s string) []string {
	var words []string
	for _, w := range strings.Fields(s) {
		w = strings.ToLower(w)
		if len(w) >= 2 {
			words = append(words, w)
		}
	}
	return words
}

// SetStalenessScore updates the staleness detection fields on a learning.
// score: 0.0 = valid, 1.0 = stale. Pass negative to leave unchanged.
func (s *Store) SetStalenessScore(id int64, score float64, reason, stalenessType string) error {
	now := fmtTime(time.Now())
	_, err := s.db.Exec(`UPDATE learnings SET staleness_score = ?, staleness_reason = ?,
		staleness_checked_at = ?, staleness_type = ?
		WHERE id = ? AND superseded_by IS NULL`,
		score, reason, now, stalenessType, id)
	if err != nil {
		return fmt.Errorf("set staleness score %d: %w", id, err)
	}
	return nil
}

// GetStalenessScores returns staleness_score for a batch of learning IDs.
// Only returns entries where staleness_score IS NOT NULL (has been checked).
func (s *Store) GetStalenessScores(ids []int64) (map[int64]float64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`SELECT id, staleness_score FROM learnings
		WHERE id IN (%s) AND staleness_score IS NOT NULL`, strings.Join(placeholders, ","))
	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]float64)
	for rows.Next() {
		var id int64
		var score float64
		if err := rows.Scan(&id, &score); err == nil {
			result[id] = score
		}
	}
	return result, rows.Err()
}

// GetStaleLearnings returns active learnings with staleness_score >= minScore.
func (s *Store) GetStaleLearnings(project string, minScore float64) ([]models.Learning, error) {
	rows, err := s.readerDB().Query(`SELECT
		l.id, l.session_id, l.category, l.content, l.project, l.confidence,
		l.superseded_by, l.supersede_reason, l.created_at, l.expires_at, l.model_used, l.source,
		COALESCE(l.hit_count, 0), COALESCE(l.emotional_intensity, 0.0), l.last_hit_at, COALESCE(l.session_flavor, ''), l.valid_until, l.supersedes, COALESCE(l.importance, 3), l.supersede_status, COALESCE(l.noise_count, 0), COALESCE(l.fail_count, 0),
		COALESCE(l.match_count, 0), COALESCE(l.inject_count, 0), COALESCE(l.use_count, 0), COALESCE(l.save_count, 0), COALESCE(l.stability, 30.0),
		COALESCE(l.context, ''), COALESCE(l.domain, 'code'), COALESCE(l.trigger_rule, ''), COALESCE(l.embedding_text, ''),
		COALESCE(l.source_file, ''), COALESCE(l.source_hash, ''), COALESCE(l.doc_chunk_ref, 0), COALESCE(l.task_type, ''), COALESCE(l.turns_at_creation, 0), COALESCE(l.origin_tool, ''), COALESCE(l.source_msg_from, -1), COALESCE(l.source_msg_to, -1),
		COALESCE(l.canonical_project, ''),
		COALESCE(l.attribution, ''),
		COALESCE(l.staleness_score, 0.0), COALESCE(l.staleness_reason, ''), l.staleness_checked_at, COALESCE(l.staleness_type, ''), COALESCE(l.code_fingerprint, ''), COALESCE(l.project_source, '')
		FROM learnings l
		WHERE l.superseded_by IS NULL
		AND l.staleness_score >= ?
		AND (l.project = ? OR l.canonical_project = ?)
		ORDER BY l.staleness_score DESC
		LIMIT 100`, minScore, project, project)
	if err != nil {
		return nil, fmt.Errorf("GetStaleLearnings: %w", err)
	}
	defer rows.Close()
	return scanLearnings(rows)
}

// StoreCodeFingerprint saves a SHA256 fingerprint of referenced code at creation time.
func (s *Store) StoreCodeFingerprint(id int64, fingerprint string) error {
	_, err := s.db.Exec(`UPDATE learnings SET code_fingerprint = ? WHERE id = ?`, fingerprint, id)
	if err != nil {
		return fmt.Errorf("StoreCodeFingerprint %d: %w", id, err)
	}
	return nil
}

// ResolveBatch marks multiple learnings as resolved in a single transaction.
func (s *Store) ResolveBatch(ids []int64, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`UPDATE learnings SET superseded_by = ?, supersede_reason = ?, valid_until = datetime('now')
		WHERE id = ? AND superseded_by IS NULL`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(SupersededByResolved, reason, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("resolve %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// SetTaskTypeBatch updates task_type for multiple learnings in a single transaction.
// Only updates learnings where superseded_by IS NULL (active items).
func (s *Store) SetTaskTypeBatch(ids []int64, taskType string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`UPDATE learnings SET task_type = ?
		WHERE id = ? AND superseded_by IS NULL`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(taskType, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("set task_type %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// CountBatchCleanup returns the count of items that were auto-resolved
// by the old resolve-stale batch process (without modifying them).
func (s *Store) CountBatchCleanup() (int64, error) {
	var count int64
	err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM learnings
		WHERE supersede_reason = ? AND superseded_by = ?`,
		BatchCleanupReason, SupersededByResolved).Scan(&count)
	return count, err
}

// ReactivateBatchCleanup clears superseded_by on items that were auto-resolved
// by the old resolve-stale batch process. Returns count of reactivated items.
func (s *Store) ReactivateBatchCleanup() (int64, error) {
	result, err := s.db.Exec(`UPDATE learnings SET superseded_by = NULL, supersede_reason = NULL, valid_until = NULL
		WHERE supersede_reason = ? AND superseded_by = ?`, BatchCleanupReason, SupersededByResolved)
	if err != nil {
		return 0, fmt.Errorf("reactivate batch-cleanup: %w", err)
	}
	return result.RowsAffected()
}

// isInSupersededChain checks if targetID appears in the supersede chain starting from startID.
// Used to prevent cycles when setting superseded_by.
func (s *Store) isInSupersededChain(startID, targetID int64) bool {
	current := startID
	for i := 0; i < 10; i++ {
		var next sql.NullInt64
		err := s.readerDB().QueryRow("SELECT superseded_by FROM learnings WHERE id = ?", current).Scan(&next)
		if err != nil || !next.Valid || next.Int64 <= 0 {
			return false
		}
		if next.Int64 == targetID {
			return true
		}
		current = next.Int64
	}
	return false
}

// SupersedeLearning marks a learning as superseded by another.
func (s *Store) SupersedeLearning(id, supersededByID int64, reason string) error {
	return s.SupersedeLearningBatch([]int64{id}, supersededByID, reason)
}

// SupersedeLearningBatch marks multiple learnings as superseded in a single transaction.
// Two UPDATEs per learning (mark + backlink) × N → 1 COMMIT instead of 2N.
func (s *Store) SupersedeLearningBatch(ids []int64, supersededByID int64, reason string) error {
	if len(ids) == 0 {
		return nil
	}

	// Cycle detection: skip any ID where the winner is already in the loser's chain,
	// or where winner == loser (self-supersede is semantically meaningless and would
	// create a self-loop that breaks GetLearningChain traversal).
	if supersededByID > 0 {
		filtered := make([]int64, 0, len(ids))
		for _, id := range ids {
			if id == supersededByID {
				log.Printf("warn: supersede self-loop rejected: %d -> %d, skipping", id, supersededByID)
				continue
			}
			if s.isInSupersededChain(supersededByID, id) {
				log.Printf("warn: supersede cycle detected: %d -> %d -> ... -> %d, skipping", id, supersededByID, id)
				continue
			}
			filtered = append(filtered, id)
		}
		ids = filtered
		if len(ids) == 0 {
			return nil
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	markStmt, err := tx.Prepare(`UPDATE learnings SET superseded_by = ?, supersede_reason = ?, valid_until = datetime('now')
		WHERE id = ? AND superseded_by IS NULL`)
	if err != nil {
		return fmt.Errorf("prepare mark: %w", err)
	}
	defer markStmt.Close()

	linkStmt, err := tx.Prepare(`UPDATE learnings SET supersedes = ? WHERE id = ? AND supersedes IS NULL`)
	if err != nil {
		return fmt.Errorf("prepare link: %w", err)
	}
	defer linkStmt.Close()

	for _, id := range ids {
		if _, err := markStmt.Exec(supersededByID, reason, id); err != nil {
			return fmt.Errorf("mark learning %d superseded by %d: %w", id, supersededByID, err)
		}
		if supersededByID > 0 {
			if _, err := linkStmt.Exec(id, supersededByID); err != nil {
				return fmt.Errorf("link learning %d to successor %d: %w", id, supersededByID, err)
			}
		}
		// Clean up AQ-FTS for superseded learning
		if _, err := tx.Exec("DELETE FROM anticipated_queries_fts WHERE learning_id = ?", id); err != nil {
			return fmt.Errorf("clear anticipated queries for %d: %w", id, err)
		}
	}

	return tx.Commit()
}

// ResolveSupersededIDs takes a list of learning IDs and returns a map from
// superseded ID → active successor. Follows chains (A→B→C) up to 10 hops.
// Active IDs (superseded_by IS NULL) are not included in the map — they need
// no redirect. A superseded ID whose chain has no active end maps to 0
// ("drop me"); callers must treat any value <= 0 as "no usable successor".
func (s *Store) ResolveSupersededIDs(ids []int64) (map[int64]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	// Single recursive CTE replaces N+1 per-hop queries.
	// Walks each supersede chain up to 10 hops, then picks the deepest
	// (= final) target per original_id.
	// The final SELECT only reports a target that is itself active
	// (superseded_by IS NULL). A chain that ends on a resolved row (negative
	// superseded_by), on a substance-free row (superseded_by = 0) or on a
	// dangling ID reports 0 instead — the caller drops those, so an inactive
	// learning can never be surfaced as an "active successor".
	query := `WITH RECURSIVE chain(original_id, current_id, depth) AS (
		SELECT id, CAST(superseded_by AS INTEGER), 1
		FROM learnings
		WHERE id IN (` + strings.Join(placeholders, ",") + `)
		AND superseded_by IS NOT NULL
		UNION ALL
		SELECT c.original_id, CAST(l.superseded_by AS INTEGER), c.depth + 1
		FROM chain c
		JOIN learnings l ON l.id = c.current_id
		WHERE l.superseded_by IS NOT NULL AND l.superseded_by > 0
		AND c.depth < 10
	)
	SELECT c.original_id,
		CASE WHEN t.id IS NOT NULL AND t.superseded_by IS NULL THEN c.current_id ELSE 0 END AS target_id
	FROM chain c
	LEFT JOIN learnings t ON t.id = c.current_id
	WHERE NOT EXISTS (
		SELECT 1 FROM chain c2
		WHERE c2.original_id = c.original_id
		AND c2.depth > c.depth
	)`

	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve superseded: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]int64)
	for rows.Next() {
		var original, target int64
		if err := rows.Scan(&original, &target); err == nil {
			result[original] = target
		}
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// GetRecentNarratives returns the N newest narrative learnings for a project.
// Only includes narratives from sessions with at least minMessages messages.
func (s *Store) GetRecentNarratives(project string, limit int) ([]models.Learning, error) {
	if limit <= 0 {
		limit = 3
	}
	canonical := s.resolveCanonicalProject(project)
	rows, err := s.readerDB().Query(`SELECT l.id, l.session_id, l.category, l.content, l.project, l.confidence,
		l.superseded_by, l.supersede_reason, l.created_at, l.expires_at, l.model_used, l.source,
		COALESCE(l.hit_count, 0), COALESCE(l.emotional_intensity, 0.0), l.last_hit_at, COALESCE(l.session_flavor, ''), l.valid_until, l.supersedes, COALESCE(l.importance, 3), l.supersede_status, COALESCE(l.noise_count, 0), COALESCE(l.fail_count, 0),
		COALESCE(l.match_count, 0), COALESCE(l.inject_count, 0), COALESCE(l.use_count, 0), COALESCE(l.save_count, 0), COALESCE(l.stability, 30.0),
		COALESCE(l.context, ''), COALESCE(l.domain, 'code'), COALESCE(l.trigger_rule, ''), COALESCE(l.embedding_text, ''),
		COALESCE(l.source_file, ''), COALESCE(l.source_hash, ''), COALESCE(l.doc_chunk_ref, 0), COALESCE(l.task_type, ''), COALESCE(l.turns_at_creation, 0), COALESCE(l.origin_tool, ''), COALESCE(l.source_msg_from, -1), COALESCE(l.source_msg_to, -1),
		COALESCE(l.canonical_project, ''),
		COALESCE(l.attribution, ''),
		COALESCE(l.staleness_score, 0.0), COALESCE(l.staleness_reason, ''), l.staleness_checked_at, COALESCE(l.staleness_type, ''), COALESCE(l.code_fingerprint, ''), COALESCE(l.project_source, '')
		FROM learnings l
		LEFT JOIN sessions s ON l.session_id = s.id
		WHERE l.category = 'narrative' AND l.canonical_project = ? AND ` + models.RecallableLearningSQLAlias + `
		AND (s.message_count IS NULL OR s.message_count = 0 OR s.message_count >= ?)
		AND (s.flavor_learnings_count != 0 OR s.flavor_learnings_count IS NULL) /* -1=unknown, >0=grounded, NULL=no session */
		ORDER BY l.created_at DESC LIMIT ?`, canonical, ShortSessionMaxMessages, limit)
	if err != nil {
		return nil, fmt.Errorf("get recent narratives: %w", err)
	}
	defer rows.Close()
	return scanLearnings(rows)
}

func scanLearnings(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]models.Learning, error) {
	var learnings []models.Learning
	for rows.Next() {
		var l models.Learning
		var createdAt string
		var expiresAt, sessionID, project, supersedeReason, source, lastHitAt, validUntil, supersedeStatus *string
		var supersededBy, supersedes *int64
		var stalenessCheckedAt *string
		var projectSource string
		if err := rows.Scan(&l.ID, &sessionID, &l.Category, &l.Content, &project, &l.Confidence,
			&supersededBy, &supersedeReason, &createdAt, &expiresAt, &l.ModelUsed, &source, &l.HitCount, &l.EmotionalIntensity, &lastHitAt, &l.SessionFlavor, &validUntil, &supersedes, &l.Importance, &supersedeStatus, &l.NoiseCount, &l.FailCount,
			&l.MatchCount, &l.InjectCount, &l.UseCount, &l.SaveCount, &l.Stability,
			&l.Context, &l.Domain, &l.TriggerRule, &l.EmbeddingText,
			&l.SourceFile, &l.SourceHash, &l.DocChunkRef, &l.TaskType, &l.TurnsAtCreation, &l.OriginTool, &l.SourceMsgFrom, &l.SourceMsgTo,
			&l.CanonicalProject, &l.Attribution,
			&l.StalenessScore, &l.StalenessReason, &stalenessCheckedAt, &l.StalenessType, &l.CodeFingerprint, &projectSource); err != nil {
			return nil, err
		}
		l.ProjectSource = projectSource
		l.CreatedAt = parseTime(createdAt)
		if expiresAt != nil {
			t := parseTime(*expiresAt)
			l.ExpiresAt = &t
		}
		if lastHitAt != nil {
			t := parseTime(*lastHitAt)
			l.LastHitAt = &t
		}
		if sessionID != nil {
			l.SessionID = *sessionID
		}
		if project != nil {
			l.Project = *project
		}
		if supersededBy != nil {
			l.SupersededBy = supersededBy
		}
		if supersedeReason != nil {
			l.SupersedeReason = *supersedeReason
		}
		if validUntil != nil {
			t := parseTime(*validUntil)
			l.ValidUntil = &t
		}
		if supersedes != nil {
			l.Supersedes = supersedes
		}
		if source != nil {
			l.Source = *source
		}
		if supersedeStatus != nil {
			l.SupersedeStatus = *supersedeStatus
		}
		if stalenessCheckedAt != nil {
			l.StalenessCheckedAt = *stalenessCheckedAt
		}
		learnings = append(learnings, l)
	}
	return learnings, rows.Err()
}

// LoadJunctionData loads entities, actions, keywords, anticipated_queries for a learning.
func (s *Store) LoadJunctionData(l *models.Learning) error {
	return s.loadJunctionData(l)
}

func (s *Store) loadJunctionData(l *models.Learning) error {
	rows, err := s.readerDB().Query("SELECT value FROM learning_entities WHERE learning_id = ?", l.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
		l.Entities = append(l.Entities, v)
	}

	rows2, err := s.readerDB().Query("SELECT value FROM learning_actions WHERE learning_id = ?", l.ID)
	if err != nil {
		return err
	}
	defer rows2.Close()
	for rows2.Next() {
		var v string
		if err := rows2.Scan(&v); err != nil {
			return err
		}
		l.Actions = append(l.Actions, v)
	}

	rows3, err := s.readerDB().Query("SELECT value FROM learning_keywords WHERE learning_id = ?", l.ID)
	if err != nil {
		return err
	}
	defer rows3.Close()
	for rows3.Next() {
		var v string
		if err := rows3.Scan(&v); err != nil {
			return err
		}
		l.Keywords = append(l.Keywords, v)
	}

	rows4, err := s.readerDB().Query("SELECT value FROM learning_anticipated_queries WHERE learning_id = ?", l.ID)
	if err != nil {
		return err
	}
	defer rows4.Close()
	for rows4.Next() {
		var v string
		if err := rows4.Scan(&v); err != nil {
			return err
		}
		l.AnticipatedQueries = append(l.AnticipatedQueries, v)
	}
	return nil
}

// BatchLoadEntities loads entities for multiple learning IDs at once.
func (s *Store) BatchLoadEntities(ids []int64) map[int64][]string {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := "SELECT learning_id, value FROM learning_entities WHERE learning_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[int64][]string)
	for rows.Next() {
		var id int64
		var v string
		if rows.Scan(&id, &v) == nil {
			result[id] = append(result[id], v)
		}
	}
	return result
}

// BatchLoadActions loads actions for multiple learning IDs at once.
func (s *Store) BatchLoadActions(ids []int64) map[int64][]string {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := "SELECT learning_id, value FROM learning_actions WHERE learning_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[int64][]string)
	for rows.Next() {
		var id int64
		var v string
		if rows.Scan(&id, &v) == nil {
			result[id] = append(result[id], v)
		}
	}
	return result
}

// supersedeIDs runs an UPDATE ... RETURNING id and returns the ids it changed.
//
// One statement on one handle, inside a transaction. The former shape — collect
// the ids on the read pool, then update on the write pool — could hand back a set
// the update never touched: a row that appeared between the two statements was
// superseded without being reported, and a vector-store cleanup that trusts the
// reported set then removed vectors of learnings that stayed active, or kept the
// vectors of ones that were gone (YM-367). RETURNING makes the returned set the
// changed set by construction, and the transaction keeps the rows it reads stable
// until the commit.
func (s *Store) supersedeIDs(query string, args ...any) ([]int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// InsertContradiction records a contradiction flagged by Claude.
func (s *Store) InsertContradiction(learningIDs string, description, project, threadID string) error {
	_, err := s.db.Exec(`
		INSERT INTO contradictions (learning_ids, description, project, thread_id)
		VALUES (?, ?, ?, ?)
	`, learningIDs, description, project, threadID)
	return err
}

// GetLearningCounts returns category → active learning count for a project.
// Excludes: unfinished (has own section), preference/relationship (in Persona section), narrative (not knowledge).
func (s *Store) GetLearningCounts(project string) (map[string]int, error) {
	canonical := s.resolveCanonicalProject(project)
	projectFilter := "1=1"
	args := []any{fmtTime(time.Now())}
	if project != "" {
		projectFilter = "(canonical_project = ? OR canonical_project = '')"
		args = append(args, canonical)
	}
	query := `SELECT category, COUNT(*) FROM learnings
		WHERE superseded_by IS NULL
		AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))
		AND category NOT IN ('unfinished', 'preference', 'relationship', 'narrative')
		AND ` + projectFilter + `
		GROUP BY category`
	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get learning counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var cat string
		var count int
		if err := rows.Scan(&cat, &count); err != nil {
			return nil, err
		}
		counts[cat] = count
	}
	return counts, rows.Err()
}

// GetLearningByContentHash finds an active learning with the given content hash.
func (s *Store) GetLearningByContentHash(hash string) (*models.Learning, error) {
	if hash == "" {
		return nil, nil
	}
	// Quarantined rows must not answer this lookup. It is the write-side dedup
	// gate: if a quarantined row matched, the same content could never be saved
	// again (the caller reports "duplicate" and inserts nothing) while nobody can
	// read the row that it matched.
	row := s.readerDB().QueryRow(`SELECT id, content, category, project, COALESCE(match_count, 0)
		FROM learnings WHERE content_hash = ? AND `+models.RecallableLearningSQL+` LIMIT 1`, hash)
	var l models.Learning
	err := row.Scan(&l.ID, &l.Content, &l.Category, &l.Project, &l.MatchCount)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// CountLearningSince returns the number of learnings created after the given timestamp.
func (s *Store) CountLearningSince(since time.Time) int {
	var count int
	s.readerDB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE julianday(created_at) > julianday(?)`, fmtTime(since)).Scan(&count)
	return count
}

// CountLearningsSinceID counts the learnings that arrived after the given ID.
// Alerts are excluded: they are derived rows and would otherwise let a detection
// run satisfy the next gate by itself.
func (s *Store) CountLearningsSinceID(afterID int64) (int, error) {
	var count int
	err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM learnings
		WHERE id > ? AND category != 'recurrence_alert'`, afterID).Scan(&count)
	return count, err
}

// GetRecentLearningIDs returns the N most recently created recallable learning IDs,
// restricted to sessionID when one is given.
// Used by hybrid_search skip_recent to prevent echo injection of just-extracted
// learnings. Quarantined and superseded rows are excluded: they must not consume
// a skip slot. Ordering is by insertion id, not created_at: an extraction that
// backdates a row to its session start is still the newest arrival.
func (s *Store) GetRecentLearningIDs(n int, sessionID string) ([]int64, error) {
	query := `SELECT id FROM learnings WHERE ` + models.RecallableLearningSQL
	args := make([]any, 0, 2)
	if sessionID != "" {
		query += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, n)

	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// GetLatestSessionID returns the most recent session ID for a project.
func (s *Store) GetLatestSessionID(project string) (string, error) {
	var sid string
	var err error
	if project != "" {
		err = s.readerDB().QueryRow(`SELECT id FROM sessions WHERE project_short = ? ORDER BY started_at DESC LIMIT 1`, project).Scan(&sid)
	} else {
		err = s.readerDB().QueryRow(`SELECT id FROM sessions ORDER BY started_at DESC LIMIT 1`).Scan(&sid)
	}
	return sid, err
}

// GetCategoriesBulk returns a map of id → category for the given IDs.
func (s *Store) GetCategoriesBulk(ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.readerDB().Query(
		`SELECT CAST(id AS TEXT), category FROM learnings WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]string, len(ids))
	for rows.Next() {
		var id, cat string
		if rows.Scan(&id, &cat) == nil {
			result[id] = cat
		}
	}
	return result, rows.Err()
}

// QuarantinedIDs returns the subset of ids that are quarantined. IDs that do not
// exist are not in the map. Callers use it as a last gate when a hit can arrive
// without passing a SQL filter (anticipated-query vectors, supersede redirects).
//
// The list is queried in batches: a caller-controlled result count must not be
// able to push the statement past SQLite's variable ceiling, which would turn
// the gate into a no-op.
func (s *Store) QuarantinedIDs(ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	const batchSize = 500
	result := make(map[string]bool)
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.readerDB().Query(
			`SELECT CAST(id AS TEXT) FROM learnings WHERE id IN (`+strings.Join(placeholders, ",")+`) AND COALESCE(quarantined_at, '') <> ''`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				result[id] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// GetSupersededChain returns the supersede history for a learning (newest first).
// Follows the supersedes backlink up to maxDepth levels deep.
func (s *Store) GetSupersededChain(id int64, maxDepth int) ([]models.Learning, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	var chain []models.Learning
	currentID := id
	for i := 0; i < maxDepth; i++ {
		l, err := s.GetLearning(currentID)
		if err != nil {
			break
		}
		chain = append(chain, *l)
		if l.Supersedes == nil {
			break
		}
		currentID = *l.Supersedes
	}
	return chain, nil
}

// GetVolatility returns how often a topic has been superseded (chain length - 1).
func (s *Store) GetVolatility(id int64) (int, error) {
	chain, err := s.GetSupersededChain(id, 10)
	if err != nil {
		return 0, err
	}
	if len(chain) <= 1 {
		return 0, nil
	}
	return len(chain) - 1, nil
}

// SetSupersedeStatus updates the supersede_status of a learning.
func (s *Store) SetSupersedeStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE learnings SET supersede_status = ? WHERE id = ?`, status, id)
	return err
}

// ForkLearning represents a learning extracted by a fork in the current session.
type ForkLearning struct {
	ID       int64
	Content  string
	Category string
	Source   string
}

// UpdateImpactScore updates the running average impact score for a learning.
func (s *Store) UpdateImpactScore(learningID int64, score float64) error {
	_, err := s.db.Exec(`UPDATE learnings SET impact_score = (COALESCE(impact_score, 0.0) * COALESCE(impact_count, 0) + ?) / (COALESCE(impact_count, 0) + 1), impact_count = COALESCE(impact_count, 0) + 1 WHERE id = ?`, score, learningID)
	return err
}

// GetForkLearnings returns all fork-extracted learnings for a session.
func (s *Store) GetForkLearnings(sessionID string) ([]ForkLearning, error) {
	rows, err := s.db.Query(`SELECT id, content, category, source FROM learnings WHERE session_id = ? AND source = 'fork' AND superseded_by IS NULL ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ForkLearning
	for rows.Next() {
		var fl ForkLearning
		if err := rows.Scan(&fl.ID, &fl.Content, &fl.Category, &fl.Source); err != nil {
			return nil, err
		}
		result = append(result, fl)
	}
	return result, nil
}

// InsertForkCoverage records that a fork has processed a message range.
//
// Coverage is a trust signal: a row that reaches the session's last message
// tells the batch pass there is nothing left to extract. The upper bound is
// therefore clamped to the session's real message count, so a caller cannot
// claim more than exists and silence extraction for the whole session. The
// lower bound must be a real index and the range must not be inverted.
func (s *Store) InsertForkCoverage(sessionID string, fromIdx, toIdx, forkIndex int) error {
	if sessionID == "" {
		return fmt.Errorf("fork coverage: session id required")
	}
	if fromIdx < 0 || toIdx < fromIdx {
		return fmt.Errorf("fork coverage: invalid range %d..%d for %s", fromIdx, toIdx, sessionID)
	}
	var messageCount int
	if err := s.db.QueryRow(`SELECT message_count FROM sessions WHERE id = ?`, sessionID).Scan(&messageCount); err == nil && messageCount > 0 {
		if toIdx > messageCount-1 {
			toIdx = messageCount - 1
		}
		if toIdx < fromIdx {
			return fmt.Errorf("fork coverage: range %d..%d starts beyond the session's %d messages", fromIdx, toIdx, messageCount)
		}
	}
	_, err := s.db.Exec(`INSERT INTO fork_coverage (session_id, from_msg_idx, to_msg_idx, fork_index) VALUES (?, ?, ?, ?)`, sessionID, fromIdx, toIdx, forkIndex)
	return err
}

// ForkCoverageIncomplete reports whether a fork-extracted session has at least
// minTail messages beyond the message ranges its forks covered. Sessions without
// coverage rows were never fork-extracted and report false, so they keep their
// extracted marker.
func (s *Store) ForkCoverageIncomplete(sessionID string, messageCount, minTail int) bool {
	if sessionID == "" || messageCount <= 0 {
		return false
	}
	if minTail < 1 {
		minTail = 1
	}
	var rows int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM fork_coverage WHERE session_id = ?`, sessionID,
	).Scan(&rows); err != nil || rows == 0 {
		return false
	}
	target := messageCount - minTail
	if target < 0 {
		target = 0
	}
	return !s.IsCoveredByFork(sessionID, 0, target)
}

// IsCoveredByFork checks if a message range is fully covered by a previous fork.
func (s *Store) IsCoveredByFork(sessionID string, fromIdx, toIdx int) bool {
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM fork_coverage WHERE session_id = ? AND from_msg_idx <= ? AND to_msg_idx >= ?`, sessionID, fromIdx, toIdx).Scan(&count)
	return count > 0
}

// provenance is the lineage a learning inherits from the session it was extracted
// from: the kind of agent that produced it and the dialog it was written in.
type provenance struct {
	agentRole string
	dialogID  int64
}

// SessionAgentRole is the role a learning from this session carries: the kind of
// agent bound to it, else its own agent_type, else "". Exported for the
// pre-admission check, which runs before the row exists (YM-74).
func (s *Store) SessionAgentRole(sessionID string) string {
	return s.sessionProvenance(sessionID).agentRole
}

// sessionProvenance resolves a learning's lineage from data the store already
// holds. The agent bound to the session names the kind of agent; a session that no
// agent row points at falls back to its own agent_type (set for subagent
// transcripts); the dialog the session takes part in is the dialog context the
// learning originated from. Every source may be absent — then the column stays
// empty rather than being guessed (YM-74).
func (s *Store) sessionProvenance(sessionID string) provenance {
	if sessionID == "" {
		return provenance{}
	}
	p := provenance{}
	if a, err := s.AgentGetAnyBySession(sessionID); err == nil && a != nil {
		p.agentRole = agentRoleFromSection(a.Section)
	}
	if p.agentRole == "" {
		if sess, err := s.GetSession(sessionID); err == nil && sess != nil {
			p.agentRole = sess.AgentType
		}
	}
	if d, err := s.GetActiveDialogForSession(sessionID); err == nil && d != nil {
		p.dialogID = d.ID
	} else if stripped := stripAgentPrefix(sessionID); stripped != sessionID {
		if d, err := s.GetActiveDialogForSession(stripped); err == nil && d != nil {
			p.dialogID = d.ID
		}
	}
	return p
}

// agentRoleFromSection names the kind of agent a section belongs to — the value
// conflict detection compares and the wiki renders: "yesloop-los-21" is a
// "yesloop" agent. Two lose of one campaign share the role on purpose, so parallel
// agents writing about the same topic are not reported as a cross-agent conflict.
func agentRoleFromSection(section string) string {
	if i := strings.Index(section, "-"); i > 0 {
		return section[:i]
	}
	return section
}

// sessionSourceAgent returns the source_agent for a session, or "claude" if unknown.
func (s *Store) sessionSourceAgent(sessionID string) string {
	var sa string
	if err := s.db.QueryRow(`SELECT source_agent FROM sessions WHERE id = ?`, sessionID).Scan(&sa); err != nil || sa == "" {
		return models.SourceAgentClaude
	}
	return models.NormalizeSourceAgent(sa)
}

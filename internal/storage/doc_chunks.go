package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/repo"
)

// invalidateChunkVecCache drops the process-wide vector snapshot. Every
// statement that adds, rewrites or removes chunks makes it stale; a search
// would otherwise answer with chunks that no longer exist.
func invalidateChunkVecCache() {
	chunkVecCache.mu.Lock()
	chunkVecCache.valid = false
	chunkVecCache.mu.Unlock()
}

// docChunkVecCache caches doc chunk embeddings in memory for fast cosine search.
// Invalidated on SetDocChunkEmbedding calls, and revalidated against a DB
// signature so a delete made by another process (CLI remove-docs, sync-docs
// --prune) cannot leave chunks searchable here.
type docChunkVecCache struct {
	mu      sync.RWMutex
	entries []cachedChunkVec
	valid   bool
	sig     string
}

type cachedChunkVec struct {
	id           int64
	sourceID     int64
	sourceName   string
	version      string
	sourceFile   string
	sourceHash   string
	headingPath  string
	content      string
	contentHash  string
	tokensApprox int
	metadata     map[string]string
	vec          []float32
}

var chunkVecCache = &docChunkVecCache{}

type DocSource struct {
	ID                int64
	Name              string
	Version           string
	Path              string
	URL               string
	Project           string
	ChunkCount        int
	LastSync          time.Time
	CreatedAt         time.Time
	IsSkill           bool
	IsRules           bool // condensed CLAUDE.md block; not a chunk source
	OriginalPath      string
	FullContent       string // Skill full text (only for is_skill=true sources)
	TriggerExtensions string // comma-separated, e.g. ".go,.mod"
	DocType           string // "reference" (auto-inject) or "style" (explicit search only)
	ExampleQuery      string // example docs_search query for plan checkpoint hint
}

// SkillInfo holds minimal skill metadata for listing.
type SkillInfo struct {
	Name        string
	Description string
}

// DocChunk represents a structured section of documentation.
type DocChunk struct {
	ID           int64
	SourceID     int64
	SourceFile   string
	SourceHash   string
	HeadingPath  string
	SectionLevel int
	Content      string
	ContentHash  string
	TokensApprox int
	Metadata     map[string]string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// DocChunkResult is a search result with score.
type DocChunkResult struct {
	DocChunk
	Score      float64
	SourceName string
	Version    string
}

// epochStamp is the last_sync a freshly registered doc source starts at. See
// UpsertDocSource.
const epochStamp = "1970-01-01 00:00:00"

// UpsertDocSource inserts or replaces a doc source keyed on (name, project).
func (s *Store) UpsertDocSource(ds *DocSource) (int64, error) {
	// Canonicalize the caller's project (git worktree -> main repo root) so a
	// write lands on the same key every project-scoped read resolves to (R3-2).
	project := repo.RootOrSelf(ds.Project)
	// Check if exists first to preserve ID on update
	var existingID int64
	err := s.readerDB().QueryRow(`SELECT id FROM doc_sources WHERE name = ? AND project = ?`, ds.Name, project).Scan(&existingID)
	if err == nil {
		// Update existing:
		// - FullContent="" (absent, the shape ingest.Run upserts with) → preserve existing value.
		//   It carries the condensed rules block and skill bodies; a doc-sync run that
		//   re-ingests a source must never clear it.
		// - FullContent="..." → overwrite
		// - TriggerExtensions="" (absent param) → preserve existing value
		// - TriggerExtensions="-" (sentinel from empty array) → clear to ""
		// - TriggerExtensions=".go,.mod" (values) → overwrite
		// - DocType="" (absent param) → preserve existing value
		// - DocType="reference"/"style" → overwrite
		// last_sync is deliberately NOT touched here: registration is not a
		// completed sync pass, and moving the stamp would hide a document whose
		// change is still waiting (see UpdateDocSourceStats for the one place
		// that stamps it).
		triggerVal := ds.TriggerExtensions
		if triggerVal == "-" {
			triggerVal = "" // sentinel → clear
		}
		_, err = s.db.Exec(`UPDATE doc_sources SET version = ?, path = ?, url = ?, chunk_count = ?, is_skill = ?, original_path = ?, full_content = CASE WHEN ? = '' THEN full_content ELSE ? END, trigger_extensions = CASE WHEN ? = '' AND ? != '-' THEN trigger_extensions ELSE ? END, doc_type = CASE WHEN ? = '' THEN doc_type ELSE ? END WHERE id = ?`,
			ds.Version, ds.Path, ds.URL, ds.ChunkCount, ds.IsSkill, ds.OriginalPath, ds.FullContent, ds.FullContent, ds.TriggerExtensions, ds.TriggerExtensions, triggerVal, ds.DocType, ds.DocType, existingID)
		if err != nil {
			return 0, fmt.Errorf("update doc_source: %w", err)
		}
		return existingID, nil
	}
	// Insert new
	docType := ds.DocType
	if docType == "" {
		docType = "reference"
	}
	// last_sync is the stamp of the last pass that saw the WHOLE tree; doc-sync
	// compares a document's mtime against it. Registration is not such a pass,
	// so a new row starts at the epoch and the first completed ingest Run stamps
	// it. Otherwise a source whose very first walk dropped an unreadable subtree
	// would look synced and never retry that subtree.
	result, err := s.db.Exec(`INSERT INTO doc_sources (name, version, path, url, project, chunk_count, is_skill, original_path, full_content, trigger_extensions, doc_type, last_sync) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ds.Name, ds.Version, ds.Path, ds.URL, project, ds.ChunkCount, ds.IsSkill, ds.OriginalPath, ds.FullContent, ds.TriggerExtensions, docType, epochStamp)
	if err != nil {
		return 0, fmt.Errorf("insert doc_source: %w", err)
	}
	return result.LastInsertId()
}

// GetDocSource retrieves a single doc source by name and project.
func (s *Store) GetDocSource(name, project string) (*DocSource, error) {
	project = repo.RootOrSelf(project)
	ds := &DocSource{}
	var lastSync, createdAt string
	var path, url, originalPath sql.NullString
	var isSkill sql.NullBool
	var isRules int64
	err := s.readerDB().QueryRow(`SELECT id, name, version, path, url, project, chunk_count, last_sync, created_at, COALESCE(is_skill, 0), COALESCE(original_path, ''), COALESCE(full_content, ''), COALESCE(trigger_extensions, ''), COALESCE(doc_type, 'reference'), COALESCE(is_rules, 0) FROM doc_sources WHERE name = ? AND project = ?`,
		name, project).Scan(&ds.ID, &ds.Name, &ds.Version, &path, &url, &ds.Project, &ds.ChunkCount, &lastSync, &createdAt, &isSkill, &originalPath, &ds.FullContent, &ds.TriggerExtensions, &ds.DocType, &isRules)
	if err != nil {
		return nil, fmt.Errorf("get doc_source %q/%q: %w", name, project, err)
	}
	ds.IsRules = isRules != 0
	if path.Valid {
		ds.Path = path.String
	}
	if url.Valid {
		ds.URL = url.String
	}
	if isSkill.Valid {
		ds.IsSkill = isSkill.Bool
	}
	if originalPath.Valid {
		ds.OriginalPath = originalPath.String
	}
	ds.LastSync = parseTime(lastSync)
	ds.CreatedAt = parseTime(createdAt)
	return ds, nil
}

// SetDocSourceOriginalPath stores the original skill path for restore on remove.
func (s *Store) SetDocSourceOriginalPath(sourceID int64, path string) {
	_, _ = s.db.Exec(`UPDATE doc_sources SET original_path = ? WHERE id = ?`, path, sourceID)
}

// GetSkillContent returns the full_content for a skill source.
func (s *Store) GetSkillContent(name, project string) (string, error) {
	project = repo.RootOrSelf(project)
	var content string
	err := s.readerDB().QueryRow(`SELECT COALESCE(full_content, '') FROM doc_sources WHERE name = ? AND project = ? AND is_skill = 1`, name, project).Scan(&content)
	if err != nil {
		return "", fmt.Errorf("get skill content %q/%q: %w", name, project, err)
	}
	return content, nil
}

// GetRulesContent returns the condensed rules block for a project (for proxy re-injection).
func (s *Store) GetRulesContent(project string) string {
	project = repo.RootOrSelf(project)
	var content string
	s.readerDB().QueryRow(`SELECT COALESCE(full_content, '') FROM doc_sources WHERE is_rules = 1 AND project = ? LIMIT 1`, project).Scan(&content)
	return content
}

// GetRulesHash returns the stored content hash for change detection.
func (s *Store) GetRulesHash(project string) string {
	project = repo.RootOrSelf(project)
	var hash string
	s.readerDB().QueryRow(`SELECT COALESCE(version, '') FROM doc_sources WHERE is_rules = 1 AND project = ? LIMIT 1`, project).Scan(&hash)
	return hash
}

// ListRulesProjects returns all projects with rules (is_rules=1), with their source path.
func (s *Store) ListRulesProjects() ([]struct{ Project, Path string }, error) {
	rows, err := s.readerDB().Query(`SELECT project, path FROM doc_sources WHERE is_rules = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []struct{ Project, Path string }
	for rows.Next() {
		var r struct{ Project, Path string }
		if rows.Scan(&r.Project, &r.Path) == nil {
			result = append(result, r)
		}
	}
	return result, nil
}

// SaveRulesContent updates the condensed rules content and hash for an existing doc_source.
func (s *Store) SaveRulesContent(sourceID int64, condensed, hash string) error {
	_, err := s.db.Exec(`UPDATE doc_sources SET is_rules = 1, full_content = ?, version = ? WHERE id = ?`, condensed, hash, sourceID)
	return err
}

// ListSkillNames returns minimal info for all skills in a project.
func (s *Store) ListSkillNames(project string) ([]SkillInfo, error) {
	project = repo.RootOrSelf(project)
	rows, err := s.readerDB().Query(`SELECT name, COALESCE(full_content, '') FROM doc_sources WHERE is_skill = 1 AND project = ? ORDER BY name`, project)
	if err != nil {
		return nil, fmt.Errorf("list skill names: %w", err)
	}
	defer rows.Close()

	var skills []SkillInfo
	for rows.Next() {
		var si SkillInfo
		var content string
		if err := rows.Scan(&si.Name, &content); err != nil {
			return nil, err
		}
		// Extract description from frontmatter if present
		si.Description = ExtractFrontmatterField(content, "description")
		skills = append(skills, si)
	}
	return skills, rows.Err()
}

// ExtractFrontmatterField extracts a field value from YAML frontmatter.
func ExtractFrontmatterField(content, field string) string {
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		return ""
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, field+":") {
			val := strings.TrimPrefix(line, field+":")
			return strings.Trim(strings.TrimSpace(val), "\"'")
		}
	}
	return ""
}

const docSourceCols = `SELECT id, name, version, path, url, project, chunk_count, last_sync, created_at, COALESCE(is_skill, 0), COALESCE(original_path, ''), COALESCE(full_content, ''), COALESCE(trigger_extensions, ''), COALESCE(doc_type, 'reference'), COALESCE(is_rules, 0) FROM doc_sources`

// ListDocSources returns all doc sources, optionally filtered by project.
// An empty project returns EVERY source across all projects — that is the
// "list everything" contract used by the CLI and list-docs. Callers that want
// project-scoped visibility must use ListDocSourcesForProject.
func (s *Store) ListDocSources(project string) ([]DocSource, error) {
	if project == "" {
		return s.queryDocSources(docSourceCols + ` ORDER BY name`)
	}
	// A git worktree caller resolves to its main repo so repo-scoped rows match.
	return s.queryDocSources(docSourceCols+` WHERE project = ? ORDER BY name`, repo.RootOrSelf(project))
}

// ListDocSourcesForProject returns global doc sources (empty project) plus those
// scoped to the given project. Project matching accepts both the canonical
// absolute path and the legacy bare-basename form, via exact string equality
// only — never a suffix match, so a foreign project cannot bleed in.
func (s *Store) ListDocSourcesForProject(project string) ([]DocSource, error) {
	// A session inside a git worktree reports the worktree path; resolve it to
	// its main repository so repo-scoped sources still match (R3-2).
	project = repo.RootOrSelf(project)
	where := ` WHERE project = '' OR project IS NULL`
	args := []any{}
	if project != "" {
		where += ` OR project = ?`
		args = append(args, project)
		if base := filepath.Base(project); base != project {
			where += ` OR project = ?`
			args = append(args, base)
		}
	}
	// Project-specific rows sort before globals of the same name so a
	// name-deduplicating caller prefers the project's own source.
	order := ` ORDER BY name, CASE WHEN project = '' OR project IS NULL THEN 1 ELSE 0 END`
	return s.queryDocSources(docSourceCols+where+order, args...)
}

func (s *Store) queryDocSources(query string, args ...any) ([]DocSource, error) {
	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list doc_sources: %w", err)
	}
	defer rows.Close()

	var sources []DocSource
	for rows.Next() {
		var ds DocSource
		var lastSync, createdAt string
		var path, url, originalPath sql.NullString
		var isSkill sql.NullBool
		var isRules int64
		if err := rows.Scan(&ds.ID, &ds.Name, &ds.Version, &path, &url, &ds.Project, &ds.ChunkCount, &lastSync, &createdAt, &isSkill, &originalPath, &ds.FullContent, &ds.TriggerExtensions, &ds.DocType, &isRules); err != nil {
			return nil, err
		}
		ds.IsRules = isRules != 0
		if path.Valid {
			ds.Path = path.String
		}
		if url.Valid {
			ds.URL = url.String
		}
		if isSkill.Valid {
			ds.IsSkill = isSkill.Bool
		}
		if originalPath.Valid {
			ds.OriginalPath = originalPath.String
		}
		ds.LastSync = parseTime(lastSync)
		ds.CreatedAt = parseTime(createdAt)
		sources = append(sources, ds)
	}
	return sources, rows.Err()
}

// GetDocSourcesByExtensions returns doc sources whose trigger_extensions
// contain any of the given file extensions.
// docType — when non-empty, filters to only sources with matching doc_type.
func (s *Store) GetDocSourcesByExtensions(exts []string, docType string) ([]DocSource, error) {
	if len(exts) == 0 {
		return nil, nil
	}

	query := `SELECT id, name, version, project, COALESCE(trigger_extensions, ''), chunk_count, COALESCE(doc_type, 'reference') FROM doc_sources WHERE trigger_extensions != ''`
	args := []any{}
	if docType != "" {
		query += ` AND COALESCE(doc_type, 'reference') = ?`
		args = append(args, docType)
	}
	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query doc_sources by extensions: %w", err)
	}
	defer rows.Close()

	extSet := make(map[string]bool, len(exts))
	for _, e := range exts {
		extSet[e] = true
	}

	var results []DocSource
	for rows.Next() {
		var ds DocSource
		if err := rows.Scan(&ds.ID, &ds.Name, &ds.Version, &ds.Project, &ds.TriggerExtensions, &ds.ChunkCount, &ds.DocType); err != nil {
			continue
		}
		for _, trigger := range strings.Split(ds.TriggerExtensions, ",") {
			if extSet[strings.TrimSpace(trigger)] {
				results = append(results, ds)
				break
			}
		}
	}
	return results, rows.Err()
}

// ListTriggerExtensions returns all unique trigger_extensions values.
// Lightweight — only reads trigger_extensions column, no full content.
func (s *Store) ListTriggerExtensions(project string) ([]string, error) {
	query := `SELECT DISTINCT trigger_extensions FROM doc_sources WHERE trigger_extensions != ''`
	args := []any{}
	if project != "" {
		query += ` AND project = ?`
		args = append(args, repo.RootOrSelf(project))
	}

	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list trigger extensions: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	var exts []string
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			continue
		}
		for _, ext := range strings.Split(raw, ",") {
			ext = strings.TrimSpace(ext)
			if ext != "" && !seen[ext] {
				seen[ext] = true
				exts = append(exts, ext)
			}
		}
	}
	return exts, rows.Err()
}

// GetDocChunksBySourceIDs returns top chunks from given source IDs,
// ordered by tokens_approx descending (prefer substantial chunks).
func (s *Store) GetDocChunksBySourceIDs(sourceIDs []int64, limit int) ([]DocChunk, error) {
	if len(sourceIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(sourceIDs))
	args := make([]any, len(sourceIDs))
	for i, id := range sourceIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	args = append(args, limit)

	// Prefer mid-size chunks (100-400 tokens) that are self-contained.
	// ORDER: chunks in sweet spot first (by proximity to 250), then others by size descending.
	query := fmt.Sprintf(`SELECT id, source_id, source_file, heading_path, content, tokens_approx FROM doc_chunks WHERE source_id IN (%s) ORDER BY CASE WHEN tokens_approx BETWEEN 100 AND 400 THEN 0 ELSE 1 END, ABS(tokens_approx - 250) ASC LIMIT ?`, strings.Join(placeholders, ","))

	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get doc chunks by source IDs: %w", err)
	}
	defer rows.Close()

	var chunks []DocChunk
	for rows.Next() {
		var c DocChunk
		if err := rows.Scan(&c.ID, &c.SourceID, &c.SourceFile, &c.HeadingPath, &c.Content, &c.TokensApprox); err != nil {
			continue
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// DeleteDocSource deletes a source and all its chunks + FTS entries.
// DeleteDocSourceResult holds info about a deleted source for post-delete actions.
type DeleteDocSourceResult struct {
	IsSkill            bool
	OriginalPath       string
	SourcePath         string
	DeletedLearningIDs []int64
}

func (s *Store) DeleteDocSource(name, project string) (*DeleteDocSourceResult, error) {
	project = repo.RootOrSelf(project)

	// One transaction for the whole cascade: the learnings derived from the
	// chunks cannot be rebuilt, so they must not disappear while the chunks
	// they reference survive a failed delete.
	//
	// BEGIN IMMEDIATE, not a deferred transaction: the first statement is a
	// read, and a deferred transaction that reads first holds a snapshot it may
	// no longer be allowed to write (SQLITE_BUSY_SNAPSHOT, which the busy
	// timeout does not retry because retrying would break that snapshot).
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for delete doc_source: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("begin delete doc_source: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var sourceID int64
	var isSkill sql.NullBool
	var originalPath, sourcePath sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT id, COALESCE(is_skill, 0), COALESCE(original_path, ''), COALESCE(path, '') FROM doc_sources WHERE name = ? AND project = ?`, name, project).Scan(&sourceID, &isSkill, &originalPath, &sourcePath)
	if err != nil {
		return nil, fmt.Errorf("find doc_source for delete: %w", err)
	}

	result := &DeleteDocSourceResult{}
	if isSkill.Valid {
		result.IsSkill = isSkill.Bool
	}
	if originalPath.Valid {
		result.OriginalPath = originalPath.String
	}
	if sourcePath.Valid {
		result.SourcePath = sourcePath.String
	}

	// Hard delete docs_extracted learnings that reference chunks from this source.
	// The chunks belong to the source id and nothing else is touched there; the
	// learnings do not, so the delete is scoped to the source's project. A row
	// that refs one of these chunks while carrying another project was not
	// derived from this source and survives (YM-443). The scope itself lives in
	// docLearningProjectScope, which the re-ingest sweeps use as well: one
	// definition for the three paths that retire doc-derived learnings.
	scopeClause, scopeArgs := docLearningProjectScope(project)
	removedLearnings := `source = 'docs_extracted' AND ` + scopeClause + `
		AND doc_chunk_ref IN (SELECT id FROM doc_chunks WHERE source_id = ?)`
	// Every statement below spells the same predicate, so the arguments are built
	// in one place: a leading value where the statement has one, then the scope,
	// then the source id.
	scope := func(lead ...any) []any {
		args := append([]any{}, lead...)
		args = append(args, scopeArgs...)
		return append(args, sourceID)
	}
	// Also collect their IDs for embedding cleanup
	var learningIDs []int64
	rows, err := conn.QueryContext(ctx, `SELECT id FROM learnings WHERE `+removedLearnings, scope()...)
	if err != nil {
		return nil, fmt.Errorf("collect docs_extracted learnings for source %d: %w", sourceID, err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan docs_extracted learning: %w", err)
		}
		learningIDs = append(learningIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("collect docs_extracted learnings: %w", err)
	}
	rows.Close()

	// Delete FTS entries first: they address the learnings by rowid. The junction
	// tables and the anticipated-query index go with them — foreign_keys is off, so
	// the declared ON DELETE CASCADE never fires and a row left behind here is
	// permanent (YM-367). The index matters most: its entries keep answering
	// searches with text of a learning that no longer exists.
	for _, lid := range learningIDs {
		if _, err := conn.ExecContext(ctx, `DELETE FROM learnings_fts WHERE rowid = ?`, lid); err != nil {
			return nil, fmt.Errorf("delete learnings_fts %d: %w", lid, err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM anticipated_queries_fts WHERE learning_id = ?`, lid); err != nil {
			return nil, fmt.Errorf("delete anticipated_queries_fts %d: %w", lid, err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM learning_anticipated_queries WHERE learning_id = ?`, lid); err != nil {
			return nil, fmt.Errorf("delete learning_anticipated_queries %d: %w", lid, err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM learning_keywords WHERE learning_id = ?`, lid); err != nil {
			return nil, fmt.Errorf("delete learning_keywords %d: %w", lid, err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM learning_entities WHERE learning_id = ?`, lid); err != nil {
			return nil, fmt.Errorf("delete learning_entities %d: %w", lid, err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM learning_actions WHERE learning_id = ?`, lid); err != nil {
			return nil, fmt.Errorf("delete learning_actions %d: %w", lid, err)
		}
	}

	// Inbound references first: a learning retired by one of the removed rows
	// must not keep a positive superseded_by into nothing, and a successor of a
	// removed row must not keep a supersedes that names no row. The predicate is
	// shared with the delete below, so no id is missed.
	if _, err := conn.ExecContext(ctx, `UPDATE learnings SET superseded_by = ? WHERE superseded_by IN (SELECT id FROM learnings WHERE `+removedLearnings+`)`,
		scope(SupersededByDeleted)...); err != nil {
		return nil, fmt.Errorf("clear inbound superseded_by for source %d: %w", sourceID, err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE learnings SET supersedes = NULL WHERE supersedes IN (SELECT id FROM learnings WHERE `+removedLearnings+`)`,
		scope()...); err != nil {
		return nil, fmt.Errorf("clear inbound supersedes for source %d: %w", sourceID, err)
	}

	// Delete learnings (hard delete, not supersede)
	if _, err := conn.ExecContext(ctx, `DELETE FROM learnings WHERE `+removedLearnings, scope()...); err != nil {
		return nil, fmt.Errorf("delete docs_extracted learnings for source %d: %w", sourceID, err)
	}

	// Delete FTS entries for chunks (content= table requires manual sync)
	if _, err := conn.ExecContext(ctx, `DELETE FROM doc_chunks_fts WHERE rowid IN (SELECT id FROM doc_chunks WHERE source_id = ?)`, sourceID); err != nil {
		return nil, fmt.Errorf("delete doc_chunks_fts: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM doc_chunks WHERE source_id = ?`, sourceID); err != nil {
		return nil, fmt.Errorf("delete doc_chunks: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM doc_sources WHERE id = ?`, sourceID); err != nil {
		return nil, fmt.Errorf("delete doc_source: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit delete doc_source: %w", err)
	}
	committed = true
	invalidateChunkVecCache()

	result.DeletedLearningIDs = learningIDs
	return result, nil
}

// InsertDocChunk inserts a chunk and syncs FTS. Metadata is stored as JSON.
func (s *Store) InsertDocChunk(chunk *DocChunk) (int64, error) {
	metaJSON := "{}"
	if len(chunk.Metadata) > 0 {
		b, err := json.Marshal(chunk.Metadata)
		if err != nil {
			return 0, fmt.Errorf("marshal metadata: %w", err)
		}
		metaJSON = string(b)
	}

	result, err := s.db.Exec(`INSERT INTO doc_chunks (source_id, source_file, source_hash, heading_path, section_level, content, content_hash, tokens_approx, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		chunk.SourceID, chunk.SourceFile, chunk.SourceHash, chunk.HeadingPath, chunk.SectionLevel, chunk.Content, chunk.ContentHash, chunk.TokensApprox, metaJSON)
	if err != nil {
		return 0, fmt.Errorf("insert doc_chunk: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}

	// Manual FTS sync (content= table has no automatic triggers)
	if _, err := s.db.Exec(`INSERT INTO doc_chunks_fts(rowid, content, heading_path, metadata) VALUES (?, ?, ?, ?)`,
		id, chunk.Content, chunk.HeadingPath, metaJSON); err != nil {
		return 0, fmt.Errorf("insert doc_chunks_fts: %w", err)
	}

	return id, nil
}

// GetDocChunksBySource returns all chunks for a source.
func (s *Store) GetDocChunksBySource(sourceID int64) ([]DocChunk, error) {
	rows, err := s.readerDB().Query(`SELECT id, source_id, source_file, source_hash, heading_path, section_level, content, content_hash, tokens_approx, metadata, created_at, updated_at FROM doc_chunks WHERE source_id = ? ORDER BY id`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("get doc_chunks by source: %w", err)
	}
	defer rows.Close()
	return scanDocChunks(rows)
}

// GetDocChunksByFile returns all chunks of one source for a specific file.
// sourceID is not optional: a single-file source always stores the relative
// path ".", so without the source filter the incremental sync would see — and
// delete — the chunks of every other single-file source.
func (s *Store) GetDocChunksByFile(sourceFile string, sourceID int64) ([]DocChunk, error) {
	rows, err := s.readerDB().Query(`SELECT id, source_id, source_file, source_hash, heading_path, section_level, content, content_hash, tokens_approx, metadata, created_at, updated_at FROM doc_chunks WHERE source_file = ? AND source_id = ? ORDER BY id`, sourceFile, sourceID)
	if err != nil {
		return nil, fmt.Errorf("get doc_chunks by file: %w", err)
	}
	defer rows.Close()
	return scanDocChunks(rows)
}

// DeleteDocChunksBySource deletes all chunks and FTS entries for a source.
func (s *Store) DeleteDocChunksBySource(sourceID int64) error {
	// FTS first, then content table
	if _, err := s.db.Exec(`DELETE FROM doc_chunks_fts WHERE rowid IN (SELECT id FROM doc_chunks WHERE source_id = ?)`, sourceID); err != nil {
		return fmt.Errorf("delete doc_chunks_fts by source: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM doc_chunks WHERE source_id = ?`, sourceID); err != nil {
		return fmt.Errorf("delete doc_chunks by source: %w", err)
	}
	invalidateChunkVecCache()
	return nil
}

// DeleteDocChunksByIDs deletes specific chunks and their FTS entries.
func (s *Store) DeleteDocChunksByIDs(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := s.db.Exec(`DELETE FROM doc_chunks_fts WHERE rowid IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("delete doc_chunks_fts by ids: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM doc_chunks WHERE id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("delete doc_chunks by ids: %w", err)
	}
	invalidateChunkVecCache()
	return nil
}

// UpdateDocSourceStats updates chunk_count and last_sync for a source.
// last_sync marks the last pass that saw the WHOLE source tree: doc-sync
// compares a document's mtime against it, so a pass that dropped an unreadable
// subtree must not move it (see UpdateDocSourceChunkCount).
func (s *Store) UpdateDocSourceStats(sourceID int64) error {
	_, err := s.db.Exec(`UPDATE doc_sources SET chunk_count = (SELECT COUNT(*) FROM doc_chunks WHERE source_id = ?), last_sync = CURRENT_TIMESTAMP WHERE id = ?`, sourceID, sourceID)
	if err != nil {
		return fmt.Errorf("update doc_source stats: %w", err)
	}
	return nil
}

// UpdateDocSourceChunkCount refreshes chunk_count only. It is the stamp-free
// half of UpdateDocSourceStats for a run that indexed what it could see but did
// not see everything.
func (s *Store) UpdateDocSourceChunkCount(sourceID int64) error {
	_, err := s.db.Exec(`UPDATE doc_sources SET chunk_count = (SELECT COUNT(*) FROM doc_chunks WHERE source_id = ?) WHERE id = ?`, sourceID, sourceID)
	if err != nil {
		return fmt.Errorf("update doc_source chunk count: %w", err)
	}
	return nil
}

// MaxDocChunkID returns the highest doc_chunks id, or 0 for an empty table. It
// is the watermark of the deletion sweep: rows above it were written after the
// caller started walking, so they belong to a document that walk could not see.
func (s *Store) MaxDocChunkID() (int64, error) {
	var id int64
	if err := s.readerDB().QueryRow(`SELECT COALESCE(MAX(id), 0) FROM doc_chunks`).Scan(&id); err != nil {
		return 0, fmt.Errorf("max doc chunk id: %w", err)
	}
	return id, nil
}

// sweepPathlessMidHook is a test seam inside SweepPathlessSourceChunks: it runs
// after the path check and before the first write, still inside the same write
// transaction. A test uses it to prove that a competing writer cannot slip a
// re-registration in between.
var sweepPathlessMidHook atomic.Pointer[func()]

// docLearningProjectScope renders the project clause that binds a doc-derived
// learning to one source's project, together with its arguments. The chunks
// belong to the source id and are always its own; the learnings are the rows
// that need the scope, because a row can reference one of these chunks while
// carrying another project.
//
// The project is matched exactly, as a path prefix and as "unstamped": upserting
// canonicalizes doc_sources.project to the repository root, while an ingest
// started in a checkout stamps its own path, so an exact match alone would leave
// the source's own rows active with a dangling doc_chunk_ref. A row without a
// project carries no evidence of another repository and stays in scope. Same
// rule as DeleteDocSource (YM-443); the two re-ingest sweeps carried the
// unscoped UPDATE (YM-445).
func docLearningProjectScope(project string) (string, []any) {
	clause := `(COALESCE(project, '') = ''`
	var args []any
	if project != "" {
		prefix := project + string(filepath.Separator)
		clause += ` OR COALESCE(project, '') = ? OR substr(COALESCE(project, ''), 1, length(?)) = ?`
		args = append(args, project, prefix, prefix)
	}
	return clause + `)`, args
}

// SweepPathlessSourceChunks deletes every chunk of a source that has no
// registered path and supersedes the learnings derived from them, in one write
// transaction. It returns the number of deleted chunks, the number of distinct
// documents they belonged to, and the ids of the retired learnings.
//
// The path check runs inside the transaction on purpose. A caller that reads the
// row, decides, and then deletes is not safe: a parallel run may re-register the
// source with a path and re-index it, and its content match keeps the existing
// chunk ids, so the rows a delete judges are the live ones. The transaction is
// BEGIN IMMEDIATE, so the write lock is taken before the check: a competing
// commit cannot land between the two. (A deferred BEGIN would only hold a read
// snapshot, and SQLite would then refuse the delete with BUSY_SNAPSHOT, which
// turns a racy sweep into a failing one instead of a correct one.) A source that
// already has a path makes this a no-op.
//
// The statements address the rows through a subquery on the source, not through
// a list of chunk ids: SQLite rejects a statement with more than 32767 bound
// variables, and one source can hold more chunks than that (the earlier
// per-document loop was bounded by a single document by accident). The write lock
// keeps that subquery stable for the whole transaction.
func (s *Store) SweepPathlessSourceChunks(sourceID, watermark int64) (chunks, files int, learnings []int64, err error) {
	if watermark == 0 {
		// Rows above the watermark belong to a concurrent pass; with no
		// watermark there is nothing this sweep may judge.
		return 0, 0, nil, nil
	}
	ctx := context.Background()
	// Pre-check before the write lock: a source with nothing under the watermark
	// has nothing to sweep, and holding the single writer connection for it would
	// block every other writer in this process. Sound because a parallel
	// re-index writes ids above the watermark (taken before this run listed the
	// sources), which this sweep would not judge anyway. The path is not read
	// here; that decision has to happen under the lock (see below).
	var pending int
	if err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM doc_chunks WHERE source_id = ? AND id <= ?`, sourceID, watermark).Scan(&pending); err != nil {
		return 0, 0, nil, fmt.Errorf("pre-check chunks of a pathless source: %w", err)
	}
	if pending == 0 {
		return 0, 0, nil, nil
	}
	// The transaction is managed on the connection, not through db.Begin: only
	// an explicit BEGIN IMMEDIATE takes the write lock up front.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("acquire write connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, 0, nil, fmt.Errorf("begin sweep transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if _, rbErr := conn.ExecContext(ctx, `ROLLBACK`); rbErr != nil {
			// The connection still carries an open transaction; handing it back to
			// the single-connection writer pool would make the next user fail with
			// "cannot start a transaction within a transaction".
			log.Printf("[storage] rollback of the pathless sweep failed: %v", rbErr)
			conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	var path, project string
	switch err := conn.QueryRowContext(ctx, `SELECT COALESCE(path, ''), COALESCE(project, '') FROM doc_sources WHERE id = ?`, sourceID).Scan(&path, &project); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, 0, nil, nil
	case err != nil:
		return 0, 0, nil, fmt.Errorf("read source path: %w", err)
	}
	if path != "" {
		return 0, 0, nil, nil
	}

	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT source_file) FROM doc_chunks WHERE source_id = ? AND id <= ?`, sourceID, watermark).Scan(&chunks, &files); err != nil {
		return 0, 0, nil, fmt.Errorf("count chunks of a pathless source: %w", err)
	}
	if chunks == 0 {
		return 0, 0, nil, nil
	}

	if hook := sweepPathlessMidHook.Load(); hook != nil {
		(*hook)()
	}

	// The chunk subquery is spelled out in every statement: it has to be stable,
	// and it is, because this transaction holds the write lock.
	const chunkSet = `SELECT id FROM doc_chunks WHERE source_id = ? AND id <= ?`
	learnScope, learnScopeArgs := docLearningProjectScope(project)
	learnRows, err := conn.QueryContext(ctx, `SELECT id FROM learnings WHERE doc_chunk_ref IN (`+chunkSet+`) AND superseded_by IS NULL AND `+learnScope,
		append([]any{sourceID, watermark}, learnScopeArgs...)...)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("collect learnings of a pathless source: %w", err)
	}
	for learnRows.Next() {
		var id int64
		if err := learnRows.Scan(&id); err != nil {
			learnRows.Close()
			return 0, 0, nil, fmt.Errorf("scan learning of a pathless source: %w", err)
		}
		learnings = append(learnings, id)
	}
	learnRows.Close()
	if err := learnRows.Err(); err != nil {
		return 0, 0, nil, err
	}
	if len(learnings) > 0 {
		if _, err := conn.ExecContext(ctx, `UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now') WHERE doc_chunk_ref IN (`+chunkSet+`) AND superseded_by IS NULL AND `+learnScope,
			append([]any{"re-ingest: document removed", sourceID, watermark}, learnScopeArgs...)...); err != nil {
			return 0, 0, nil, fmt.Errorf("supersede learnings of a pathless source: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx, `DELETE FROM doc_chunks_fts WHERE rowid IN (`+chunkSet+`)`, sourceID, watermark); err != nil {
		return 0, 0, nil, fmt.Errorf("delete doc_chunks_fts of a pathless source: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM doc_chunks WHERE source_id = ? AND id <= ?`, sourceID, watermark); err != nil {
		return 0, 0, nil, fmt.Errorf("delete chunks of a pathless source: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, 0, nil, fmt.Errorf("commit sweep of a pathless source: %w", err)
	}
	committed = true
	invalidateChunkVecCache()
	return chunks, files, learnings, nil
}

// sweepDocMidHook is a test seam inside SweepDocChunksByFile: it runs after the chunk
// ids are resolved and before the first write, still inside the same write transaction.
// A test uses it to prove that a competing pass cannot slip a re-index in between.
var sweepDocMidHook atomic.Pointer[func()]

// SweepDocChunksByFile deletes the chunks of one indexed document and supersedes the
// learnings derived from them, in one write transaction. It returns the number of
// chunks removed and the ids of the retired learnings. Rows above watermark belong to a
// concurrent pass and are left alone.
//
// The three steps are one transaction because a failure in any of them used to leave a
// state nothing repairs: learnings retired while the document was still served, or
// chunks gone while their learnings stayed active (YM-395). The chunks are addressed
// through a subquery on (source, document) instead of a bound id list: SQLite rejects a
// statement with more than 32767 bound variables, and one document can hold more chunks
// than that.
func (s *Store) SweepDocChunksByFile(sourceID int64, sourceFile string, watermark int64, reason string) (chunks int, learnings []int64, err error) {
	if watermark == 0 || sourceFile == "" {
		return 0, nil, nil
	}
	ctx := context.Background()
	// Pre-check before the write lock: a document with nothing under the watermark has
	// nothing to remove, and holding the single writer connection for it would block
	// every other writer in this process. A parallel re-index writes ids above the
	// watermark, which this sweep would not judge anyway.
	var pending int
	if err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM doc_chunks WHERE source_id = ? AND source_file = ? AND id <= ?`,
		sourceID, sourceFile, watermark).Scan(&pending); err != nil {
		return 0, nil, fmt.Errorf("pre-check chunks of %q: %w", sourceFile, err)
	}
	if pending == 0 {
		return 0, nil, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("acquire write connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, nil, fmt.Errorf("begin document sweep transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if _, rbErr := conn.ExecContext(ctx, `ROLLBACK`); rbErr != nil {
			// The connection still carries an open transaction; handing it back to the
			// single-connection writer pool would make the next user fail with
			// "cannot start a transaction within a transaction".
			log.Printf("[storage] rollback of the document sweep failed: %v", rbErr)
			conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	// The chunk set is spelled out in every statement: it has to be stable, and it is,
	// because this transaction holds the write lock.
	const chunkSet = `SELECT id FROM doc_chunks WHERE source_id = ? AND source_file = ? AND id <= ?`
	var project string
	switch err := conn.QueryRowContext(ctx, `SELECT COALESCE(project, '') FROM doc_sources WHERE id = ?`, sourceID).Scan(&project); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil, nil
	case err != nil:
		return 0, nil, fmt.Errorf("read project of source %d: %w", sourceID, err)
	}
	learnScope, learnScopeArgs := docLearningProjectScope(project)
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM doc_chunks WHERE source_id = ? AND source_file = ? AND id <= ?`,
		sourceID, sourceFile, watermark).Scan(&chunks); err != nil {
		return 0, nil, fmt.Errorf("count chunks of %q: %w", sourceFile, err)
	}
	if chunks == 0 {
		return 0, nil, nil
	}

	if hook := sweepDocMidHook.Load(); hook != nil {
		(*hook)()
	}

	learnRows, err := conn.QueryContext(ctx, `SELECT id FROM learnings WHERE doc_chunk_ref IN (`+chunkSet+`) AND superseded_by IS NULL AND `+learnScope,
		append([]any{sourceID, sourceFile, watermark}, learnScopeArgs...)...)
	if err != nil {
		return 0, nil, fmt.Errorf("collect learnings of %q: %w", sourceFile, err)
	}
	for learnRows.Next() {
		var id int64
		if err := learnRows.Scan(&id); err != nil {
			learnRows.Close()
			return 0, nil, fmt.Errorf("scan learning of %q: %w", sourceFile, err)
		}
		learnings = append(learnings, id)
	}
	learnRows.Close()
	if err := learnRows.Err(); err != nil {
		return 0, nil, err
	}
	if len(learnings) > 0 {
		if _, err := conn.ExecContext(ctx, `UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now') WHERE doc_chunk_ref IN (`+chunkSet+`) AND superseded_by IS NULL AND `+learnScope,
			append([]any{reason, sourceID, sourceFile, watermark}, learnScopeArgs...)...); err != nil {
			return 0, nil, fmt.Errorf("supersede learnings of %q: %w", sourceFile, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM doc_chunks_fts WHERE rowid IN (`+chunkSet+`)`, sourceID, sourceFile, watermark); err != nil {
		return 0, nil, fmt.Errorf("delete doc_chunks_fts of %q: %w", sourceFile, err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM doc_chunks WHERE source_id = ? AND source_file = ? AND id <= ?`,
		sourceID, sourceFile, watermark); err != nil {
		return 0, nil, fmt.Errorf("delete chunks of %q: %w", sourceFile, err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, nil, fmt.Errorf("commit sweep of %q: %w", sourceFile, err)
	}
	committed = true
	invalidateChunkVecCache()
	return chunks, learnings, nil
}

// GetDocChunkFiles returns the distinct document paths of one source, as stored
// in doc_chunks (relative to the source's base path). It is the index side of
// the deletion sweep: a path that is in here but not in a complete walk belongs
// to a document that no longer exists.
func (s *Store) GetDocChunkFiles(sourceID int64) ([]string, error) {
	rows, err := s.readerDB().Query(`SELECT DISTINCT source_file FROM doc_chunks WHERE source_id = ? ORDER BY source_file`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("get doc chunk files: %w", err)
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var file string
		if err := rows.Scan(&file); err != nil {
			return nil, fmt.Errorf("scan doc chunk file: %w", err)
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate doc chunk files: %w", err)
	}
	return files, nil
}

// UpdateDocChunksSourceHashByFile refreshes the file-level source hash on the
// chunks of one file of one source. See GetDocChunksByFile for why sourceID is
// part of the key.
func (s *Store) UpdateDocChunksSourceHashByFile(sourceFile string, sourceID int64, sourceHash string) error {
	_, err := s.db.Exec(`UPDATE doc_chunks SET source_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE source_file = ? AND source_id = ?`, sourceHash, sourceFile, sourceID)
	if err != nil {
		return fmt.Errorf("update doc_chunks source_hash by file: %w", err)
	}
	return nil
}

// SearchDocChunksFTS performs FTS5 search with BM25 ranking and AND-matching.
// source filters by doc_sources.name, section filters by heading_path LIKE.
// sourceIDs — when non-empty, restricts results to those source IDs.
func (s *Store) SearchDocChunksFTS(query string, source string, section string, since string, before string, limit int, sourceIDs []int64) ([]DocChunkResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if query == "" {
		return nil, nil
	}
	// The storage layer owns the SQL comparison, so it must not turn an unreadable
	// bound into "no rows" on its own; the caller gets the reason instead.
	if err := validateFilterBounds(since, before); err != nil {
		return nil, err
	}

	// Tokenize and quote for FTS5
	tokens := strings.Fields(query)
	quoted := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		tok = strings.ReplaceAll(tok, `"`, `""`)
		quoted = append(quoted, `"`+tok+`"`)
	}
	if len(quoted) == 0 {
		return nil, nil
	}

	// Build base WHERE clause (excluding MATCH)
	baseWhere := ""
	baseArgs := []any{}
	if source != "" {
		baseWhere += ` AND ds.name = ?`
		baseArgs = append(baseArgs, source)
	}
	if section != "" {
		baseWhere += ` AND dc.heading_path LIKE ?`
		baseArgs = append(baseArgs, "%"+section+"%")
	}
	// julianday() on both sides: created_at carries the writer's spelling (the
	// CURRENT_TIMESTAMP default without a 'T', RFC3339 with an offset when a
	// caller stamps it), and a lexical compare against an RFC3339 bound drops a
	// whole day on the separator alone. The bound is normalized first, so the
	// caller's spelling never reaches SQLite.
	if since != "" {
		baseWhere += ` AND julianday(dc.created_at) >= julianday(?)`
		baseArgs = append(baseArgs, SQLiteFilterTime(since))
	}
	if before != "" {
		baseWhere += ` AND julianday(dc.created_at) < julianday(?)`
		baseArgs = append(baseArgs, SQLiteFilterTime(before))
	}
	if len(sourceIDs) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(sourceIDs)), ",")
		baseWhere += ` AND dc.source_id IN (` + placeholders + `)`
		for _, id := range sourceIDs {
			baseArgs = append(baseArgs, id)
		}
	}

	runFTS := func(terms []string) ([]DocChunkResult, error) {
		ftsQuery := strings.Join(terms, " AND ")
		q := `SELECT dc.id, dc.source_id, dc.source_file, dc.source_hash, dc.heading_path, dc.section_level, dc.content, dc.content_hash, dc.tokens_approx, dc.metadata, dc.created_at, dc.updated_at, bm25(doc_chunks_fts) AS score, ds.name, ds.version
			FROM doc_chunks_fts fts
			JOIN doc_chunks dc ON dc.id = fts.rowid
			JOIN doc_sources ds ON ds.id = dc.source_id
			WHERE doc_chunks_fts MATCH ?` + baseWhere + ` ORDER BY score LIMIT ?`
		args := append([]any{ftsQuery}, baseArgs...)
		args = append(args, limit)

		rows, err := s.readerDB().Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("search doc_chunks fts: %w", err)
		}
		defer rows.Close()

		var results []DocChunkResult
		for rows.Next() {
			var r DocChunkResult
			var metaJSON, createdAt, updatedAt string
			if err := rows.Scan(&r.ID, &r.SourceID, &r.SourceFile, &r.SourceHash, &r.HeadingPath, &r.SectionLevel, &r.Content, &r.ContentHash, &r.TokensApprox, &metaJSON, &createdAt, &updatedAt, &r.Score, &r.SourceName, &r.Version); err != nil {
				return nil, err
			}
			r.CreatedAt = parseTime(createdAt)
			r.UpdatedAt = parseTime(updatedAt)
			r.Metadata = make(map[string]string)
			if metaJSON != "" && metaJSON != "{}" {
				json.Unmarshal([]byte(metaJSON), &r.Metadata)
			}
			results = append(results, r)
		}
		return results, rows.Err()
	}

	// Short queries (1-2 terms): skip existence filter, search directly
	if len(quoted) <= 2 {
		return runFTS(quoted)
	}

	// Term-existence filter: check each term against corpus, keep only terms that exist.
	// Sorts surviving terms by document frequency ascending (rarest first = most specific).
	type termHit struct {
		quoted string
		count  int
	}
	var alive []termHit
	db := s.readerDB()
	for _, q := range quoted {
		var cnt int
		err := db.QueryRow(`SELECT COUNT(*) FROM doc_chunks_fts WHERE doc_chunks_fts MATCH ?`, q).Scan(&cnt)
		if err != nil || cnt == 0 {
			continue
		}
		alive = append(alive, termHit{q, cnt})
	}
	if len(alive) < 2 {
		// Fallback: if only 1 term survives, search with that single term
		if len(alive) == 1 {
			return runFTS([]string{alive[0].quoted})
		}
		return nil, nil
	}

	// Sort by frequency ascending — rarest terms first (highest IDF proxy).
	// When we drop terms for lower tiers, we lose the most common (least specific) ones.
	sort.Slice(alive, func(i, j int) bool {
		return alive[i].count < alive[j].count
	})

	// Cap at 6 terms (research: optimal 3-6 terms for BM25)
	if len(alive) > 6 {
		alive = alive[:6]
	}

	filtered := make([]string, len(alive))
	for i, a := range alive {
		filtered[i] = a.quoted
	}

	// Tier fallback on filtered terms: all → all-1 → ... → 2
	for n := len(filtered); n >= 2; n-- {
		results, err := runFTS(filtered[:n])
		if err != nil {
			return nil, err
		}
		if len(results) > 0 {
			return results, nil
		}
	}

	return nil, nil
}

// GetLearningsBySourceFile returns active learnings linked to a source file.
func (s *Store) GetLearningsBySourceFile(sourceFile string) ([]models.Learning, error) {
	rows, err := s.readerDB().Query(`SELECT id, session_id, category, content, project, confidence,
		superseded_by, supersede_reason, created_at, expires_at, model_used, source,
		COALESCE(hit_count, 0), COALESCE(emotional_intensity, 0.0), last_hit_at, COALESCE(session_flavor, ''), valid_until, supersedes, COALESCE(importance, 3), supersede_status, COALESCE(noise_count, 0), COALESCE(fail_count, 0),
		COALESCE(match_count, 0), COALESCE(inject_count, 0), COALESCE(use_count, 0), COALESCE(save_count, 0), COALESCE(stability, 30.0),
		COALESCE(context, ''), COALESCE(domain, 'code'), COALESCE(trigger_rule, ''), COALESCE(embedding_text, ''),
		COALESCE(source_file, ''), COALESCE(source_hash, ''), COALESCE(doc_chunk_ref, 0), COALESCE(task_type, ''), COALESCE(turns_at_creation, 0), COALESCE(origin_tool, ''), COALESCE(source_msg_from, -1), COALESCE(source_msg_to, -1),
		COALESCE(canonical_project, ''),
		COALESCE(attribution, ''),
		COALESCE(staleness_score, 0.0), COALESCE(staleness_reason, ''), staleness_checked_at, COALESCE(staleness_type, ''), COALESCE(code_fingerprint, ''), COALESCE(project_source, '')
		FROM learnings WHERE source_file = ? AND superseded_by IS NULL`, sourceFile)
	if err != nil {
		return nil, fmt.Errorf("get learnings by source_file %q: %w", sourceFile, err)
	}
	defer rows.Close()
	return scanLearnings(rows)
}

// SupersedeBySourceFile marks all active learnings for a source file as superseded.
// Returns the IDs of affected learnings.
func (s *Store) SupersedeBySourceFile(sourceFile, reason string) ([]int64, error) {
	ids, err := s.supersedeIDs(`UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now')
		WHERE source_file = ? AND superseded_by IS NULL
		RETURNING id`,
		reason, sourceFile)
	if err != nil {
		return nil, fmt.Errorf("supersede by source_file %q: %w", sourceFile, err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, nil
}

// SupersedeByDocChunkRefs marks active learnings for the given chunk refs as superseded.
// This is the path every re-ingest of a changed document takes, so it is the one
// that fires in normal use, not only on cleanup. The chunks belong to sourceID
// and are always its own; the learnings are the rows that need the scope,
// because a row can reference one of these chunks while carrying another
// project. Same rule as DeleteDocSource and the two re-ingest sweeps
// (YM-443, YM-445): a re-ingest in one project must not retire the learnings of
// another (YM-452). The scope is read from the source, not from the caller's
// project: doc_sources.project is canonicalized, and only
// docLearningProjectScope matches both the canonical root and the raw checkout
// path that rows written before YM-445 still carry.
func (s *Store) SupersedeByDocChunkRefs(sourceID int64, chunkRefs []int64, reason string) ([]int64, error) {
	if len(chunkRefs) == 0 {
		return nil, nil
	}
	var project string
	switch err := s.readerDB().QueryRow(`SELECT COALESCE(project, '') FROM doc_sources WHERE id = ?`, sourceID).Scan(&project); {
	case errors.Is(err, sql.ErrNoRows):
		// No source row, no scope to bind the learnings to.
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read project of source %d: %w", sourceID, err)
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(chunkRefs)), ",")
	args := make([]any, 0, len(chunkRefs)+1)
	for _, id := range chunkRefs {
		args = append(args, id)
	}

	// Bind order follows the statement: the reason leads, then the chunk refs,
	// then the scope arguments.
	scope, scopeArgs := docLearningProjectScope(project)
	updateArgs := make([]any, 0, len(chunkRefs)+len(scopeArgs)+1)
	updateArgs = append(updateArgs, reason)
	updateArgs = append(updateArgs, args...)
	updateArgs = append(updateArgs, scopeArgs...)
	ids, err := s.supersedeIDs(`UPDATE learnings SET superseded_by = -1, supersede_reason = ?, valid_until = datetime('now')
		WHERE doc_chunk_ref IN (`+placeholders+`) AND superseded_by IS NULL AND `+scope+`
		RETURNING id`, updateArgs...)
	if err != nil {
		return nil, fmt.Errorf("supersede by doc_chunk_ref: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, nil
}

// scanDocChunks scans rows into DocChunk slices.
func scanDocChunks(rows *sql.Rows) ([]DocChunk, error) {
	var chunks []DocChunk
	for rows.Next() {
		var c DocChunk
		var metaJSON, createdAt, updatedAt string
		if err := rows.Scan(&c.ID, &c.SourceID, &c.SourceFile, &c.SourceHash, &c.HeadingPath, &c.SectionLevel, &c.Content, &c.ContentHash, &c.TokensApprox, &metaJSON, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(createdAt)
		c.UpdatedAt = parseTime(updatedAt)
		c.Metadata = make(map[string]string)
		if metaJSON != "" && metaJSON != "{}" {
			json.Unmarshal([]byte(metaJSON), &c.Metadata)
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// escapeFTS5 escapes special characters for FTS5 MATCH queries.
// Wraps each token in double quotes to treat them as literals.
func escapeFTS5(query string) string {
	tokens := strings.Fields(query)
	if len(tokens) == 0 {
		return query
	}
	escaped := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		// Replace double quotes within the token
		tok = strings.ReplaceAll(tok, `"`, `""`)
		escaped = append(escaped, `"`+tok+`"`)
	}
	return strings.Join(escaped, " ")
}

// SetDocChunkEmbedding stores an embedding vector for a doc chunk.
func (s *Store) SetDocChunkEmbedding(chunkID int64, vector []float32, contentHash string) error {
	blob := float32ToBytes(vector)
	// The millisecond stamp is what makes an in-place rewrite visible to another
	// process: it is part of chunkVecSignature, and CURRENT_TIMESTAMP's second
	// resolution would let two rewrites inside one second look like no change.
	_, err := s.db.Exec(`UPDATE doc_chunks SET embedding_vector = ?, embedding_hash = ?, updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now') WHERE id = ?`, blob, contentHash, chunkID)
	if err == nil {
		invalidateChunkVecCache()
	}
	return err
}

// DocChunksWithoutEmbedding returns chunks that need embedding (no vector or hash mismatch).
func (s *Store) DocChunksWithoutEmbedding() ([]DocChunk, error) {
	rows, err := s.readerDB().Query(`SELECT id, source_id, source_file, source_hash, heading_path, section_level, content, content_hash, tokens_approx, metadata, created_at, updated_at FROM doc_chunks WHERE embedding_vector IS NULL OR embedding_hash = '' OR embedding_hash != content_hash ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("doc chunks without embedding: %w", err)
	}
	defer rows.Close()
	return scanDocChunks(rows)
}

// SearchDocChunksSemantic performs vector similarity search on doc chunks.
// When source is non-empty, only chunks from that doc source are considered.
// sourceIDs — when non-empty, restricts the search to those source IDs (the same
// filter SearchDocChunksFTS takes, so scoping cannot be skipped on this path).
// since/before bound created_at exactly as on the BM25 lane: both are part of the
// documented docs_search contract, and a lane that ignores them would put chunks
// outside the window into the merged result.
func (s *Store) SearchDocChunksSemantic(queryVec []float32, limit int, source string, sourceIDs []int64, since, before string) ([]DocChunkResult, error) {
	if limit <= 0 {
		limit = 10
	}
	if err := validateFilterBounds(since, before); err != nil {
		return nil, err
	}

	// The window is resolved to ids once: the scored entries below come from the
	// in-process snapshot and carry no timestamp, and the alternative — one
	// statement per candidate — would pay a query per chunk.
	var windowIDs map[int64]bool
	if since != "" || before != "" {
		query := `SELECT id FROM doc_chunks WHERE 1 = 1`
		args := make([]any, 0, 2)
		if since != "" {
			query += ` AND julianday(created_at) >= julianday(?)`
			args = append(args, SQLiteFilterTime(since))
		}
		if before != "" {
			query += ` AND julianday(created_at) < julianday(?)`
			args = append(args, SQLiteFilterTime(before))
		}
		rows, err := s.readerDB().Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("doc chunk window: %w", err)
		}
		windowIDs = make(map[int64]bool)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("doc chunk window: %w", err)
			}
			windowIDs[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("doc chunk window: %w", err)
		}
	}

	// Load cache if invalid
	entries := s.loadChunkVecCache()

	var allowed map[int64]bool
	if len(sourceIDs) > 0 {
		allowed = make(map[int64]bool, len(sourceIDs))
		for _, id := range sourceIDs {
			allowed[id] = true
		}
	}

	type scored struct {
		result DocChunkResult
		sim    float64
	}
	var candidates []scored

	for _, e := range entries {
		if source != "" && e.sourceName != source {
			continue
		}
		if allowed != nil && !allowed[e.sourceID] {
			continue
		}
		if windowIDs != nil && !windowIDs[e.id] {
			continue
		}
		sim := cosineSimilarity32(queryVec, e.vec)
		if sim < 0.3 {
			continue
		}
		r := DocChunkResult{
			DocChunk: DocChunk{
				ID:           e.id,
				SourceID:     e.sourceID,
				SourceFile:   e.sourceFile,
				SourceHash:   e.sourceHash,
				HeadingPath:  e.headingPath,
				Content:      e.content,
				ContentHash:  e.contentHash,
				TokensApprox: e.tokensApprox,
				Metadata:     e.metadata,
			},
			Score:      sim,
			SourceName: e.sourceName,
			Version:    e.version,
		}
		candidates = append(candidates, scored{r, sim})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].sim > candidates[j].sim
	})

	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	results := make([]DocChunkResult, len(candidates))
	for i, c := range candidates {
		results[i] = c.result
	}
	return results, nil
}

// loadChunkVecCache returns cached vectors, loading from DB if cache is invalid.
func (s *Store) loadChunkVecCache() []cachedChunkVec {
	// Cheap fingerprint of the rows the snapshot was built from. Each search
	// pays one COUNT instead of trusting a flag that another process cannot set.
	sig := s.chunkVecSignature()

	chunkVecCache.mu.RLock()
	if chunkVecCache.valid && sig != "" && chunkVecCache.sig == sig {
		entries := chunkVecCache.entries
		chunkVecCache.mu.RUnlock()
		return entries
	}
	chunkVecCache.mu.RUnlock()

	// Upgrade to write lock and reload
	chunkVecCache.mu.Lock()
	defer chunkVecCache.mu.Unlock()

	// Double-check after acquiring write lock
	if chunkVecCache.valid && sig != "" && chunkVecCache.sig == sig {
		return chunkVecCache.entries
	}

	rows, err := s.readerDB().Query(`SELECT dc.id, dc.source_id, dc.source_file, dc.source_hash, dc.heading_path, dc.content, dc.content_hash, dc.tokens_approx, dc.metadata, dc.embedding_vector, ds.name, ds.version
		FROM doc_chunks dc
		JOIN doc_sources ds ON ds.id = dc.source_id
		WHERE dc.embedding_vector IS NOT NULL`)
	if err != nil {
		log.Printf("loadChunkVecCache: %v", err)
		return nil
	}
	defer rows.Close()

	var entries []cachedChunkVec
	for rows.Next() {
		var e cachedChunkVec
		var metaJSON string
		var vecBlob []byte
		if err := rows.Scan(&e.id, &e.sourceID, &e.sourceFile, &e.sourceHash, &e.headingPath, &e.content, &e.contentHash, &e.tokensApprox, &metaJSON, &vecBlob, &e.sourceName, &e.version); err != nil {
			log.Printf("loadChunkVecCache scan: %v", err)
			continue
		}
		if len(vecBlob) == 0 {
			continue
		}
		e.vec = bytesToFloat32(vecBlob)
		e.metadata = make(map[string]string)
		if metaJSON != "" && metaJSON != "{}" {
			json.Unmarshal([]byte(metaJSON), &e.metadata)
		}
		entries = append(entries, e)
	}

	chunkVecCache.entries = entries
	chunkVecCache.valid = true
	chunkVecCache.sig = sig
	log.Printf("loadChunkVecCache: loaded %d vectors", len(entries))
	return entries
}

// chunkVecSignature fingerprints the embedded chunks. AUTOINCREMENT never
// reuses ids, so count plus highest id changes on every insert and delete. A
// re-embedding of an existing chunk changes neither, so the newest updated_at
// rides along: a rewrite made by another process bumps it, and without it this
// process kept serving the superseded vector (YM-294). The watermark is only as
// good as the clock: a row stamped in the future holds the maximum up, and the
// cache then needs a restart. A monotonic write counter would be clock-free and
// is the follow-up worth taking if this ever bites.
func (s *Store) chunkVecSignature() string {
	var count, maxID int64
	var newestWrite string
	err := s.readerDB().QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), 0), COALESCE(MAX(updated_at), '') FROM doc_chunks WHERE embedding_vector IS NOT NULL`).Scan(&count, &maxID, &newestWrite)
	if err != nil {
		// No signature means no reuse: rebuild rather than serve a stale snapshot.
		return ""
	}
	return fmt.Sprintf("%d:%d:%s", count, maxID, newestWrite)
}

// GetReferenceSources returns reference doc sources for a project: global
// (empty project) sources plus those scoped to the given project, matched by
// canonical absolute path or legacy bare basename (exact equality only).
// Used by plan checkpoint to build the project-scoped docs-available reminder.
func (s *Store) GetReferenceSources(project string) ([]DocSource, error) {
	project = repo.RootOrSelf(project)
	where := ` WHERE doc_type = 'reference' AND is_skill = 0 AND is_rules = 0 AND (project = '' OR project IS NULL`
	args := []any{}
	if project != "" {
		where += ` OR project = ?`
		args = append(args, project)
		if base := filepath.Base(project); base != project {
			where += ` OR project = ?`
			args = append(args, base)
		}
	}
	where += `) GROUP BY name ORDER BY name`
	rows, err := s.readerDB().Query(`SELECT name, version, COALESCE(example_query, '') FROM doc_sources`+where, args...)
	if err != nil {
		return nil, fmt.Errorf("get reference sources: %w", err)
	}
	defer rows.Close()

	var sources []DocSource
	for rows.Next() {
		var ds DocSource
		if err := rows.Scan(&ds.Name, &ds.Version, &ds.ExampleQuery); err != nil {
			return nil, fmt.Errorf("scan reference source: %w", err)
		}
		sources = append(sources, ds)
	}
	return sources, rows.Err()
}

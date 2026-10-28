package embedding

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/carsteneu/yesmem/internal/models"
)

// searchAllowedBatch bounds the placeholders one statement may bind. SQLite
// rejects a statement above 32766 variables, so a caller with more allowed IDs
// than that must be spread over several statements.
const searchAllowedBatch = 500

// projectMatchesTolerant reports whether a learning belongs to the requested project,
// accepting any of the four caller/storage combinations (short name, full path, worktree
// path on either side). Mirrors storage.projectMatchesTolerant — duplicated here to
// avoid an embedding→storage import cycle. See that function for the full rules.
// Empty requested matches anything; empty stored values match any requested (legacy rows).
// Direct equality short-circuits. Otherwise a row that names its repository by PATH is
// compared through that path: two paths are the same project when their cluster keys agree,
// which folds a repository's worktrees onto its root while two repositories that share a
// basename stay apart (YM-424).
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

// VectorDoc represents a document to store in the vector store.
type VectorDoc struct {
	ID        string
	Content   string
	Embedding []float32
	Metadata  map[string]string
}

// SearchResult represents a search result from the vector store.
type SearchResult struct {
	ID         string
	Content    string
	Similarity float32
	Metadata   map[string]string
}

// VectorStore provides vector similarity search over embeddings
// stored in the learnings table (embedding_vector BLOB column).
// Supports brute-force (default) and IVF index for large datasets.
// Also holds in-memory vectors for anticipated queries (AQs) that don't map to learning rows.
type VectorStore struct {
	db       *sql.DB
	dim      int
	ivfIndex IVFSearcher // nil = brute-force
	mu       sync.RWMutex
	// In-memory storage for AQ vectors (not in learnings table)
	memDocs []VectorDoc
	// Periodic IVF save
	ivfSavePath       string
	ivfSaveInterval   int // save after this many Add() calls; 0 = disabled
	addsSinceLastSave int
}

// IVFSearcher is the interface for plugging in an IVF index.
type IVFSearcher interface {
	Search(ctx context.Context, query []float32, topK int) ([]SearchResult, error)
	Add(id uint64, vec []float32)
	Remove(id uint64)
}

// NewVectorStore creates a vector store backed by the shared SQLite DB.
// Vectors are stored in learnings.embedding_vector as little-endian float32 BLOBs.
func NewVectorStore(db *sql.DB, dimensions int) (*VectorStore, error) {
	return &VectorStore{db: db, dim: dimensions}, nil
}

// SetIVFIndex activates IVF search. Pass nil to fall back to brute-force.
func (s *VectorStore) SetIVFIndex(idx IVFSearcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ivfIndex = idx
}

// SaveIVF persists the IVF index to disk if active and supports saving.
// No-op if no IVF index is set.
func (s *VectorStore) SaveIVF(path string) error {
	s.mu.RLock()
	idx := s.ivfIndex
	s.mu.RUnlock()

	type saver interface{ Save(string) error }
	if sv, ok := idx.(saver); ok {
		return sv.Save(path)
	}
	return nil
}

// SetIVFSavePath configures periodic IVF saves after every interval Add() calls.
func (s *VectorStore) SetIVFSavePath(path string, interval int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ivfSavePath = path
	s.ivfSaveInterval = interval
}

// Add stores an embedding for a learning ID.
// Also updates IVF index if active.
func (s *VectorStore) Add(ctx context.Context, doc VectorDoc) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE learnings SET embedding_vector = ?, embedding_status = 'done' WHERE id = ?`,
		SerializeFloat32(doc.Embedding), doc.ID)
	if err != nil {
		return err
	}

	if s.ivfIndex != nil && doc.Embedding != nil {
		var id uint64
		fmt.Sscanf(doc.ID, "%d", &id)
		s.ivfIndex.Add(id, doc.Embedding)
		s.addsSinceLastSave++
		if s.ivfSaveInterval > 0 && s.addsSinceLastSave >= s.ivfSaveInterval {
			s.addsSinceLastSave = 0
			type saver interface{ Save(string) error }
			if sv, ok := s.ivfIndex.(saver); ok && s.ivfSavePath != "" {
				sv.Save(s.ivfSavePath)
			}
		}
	}
	return nil
}

// AddBatch stores embeddings for multiple learning IDs in a single transaction.
// Documents with non-numeric IDs (e.g. "89_aq_0") are stored in-memory only.
func (s *VectorStore) AddBatch(ctx context.Context, docs []VectorDoc) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Separate DB-backed (learning content) from in-memory (AQs)
	var dbDocs []VectorDoc
	for _, doc := range docs {
		if strings.Contains(doc.ID, "_aq_") {
			s.memDocs = append(s.memDocs, doc)
		} else {
			dbDocs = append(dbDocs, doc)
		}
	}

	if len(s.memDocs) > 0 && len(dbDocs) == 0 {
		// Pure AQ batch — nothing to write to DB
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`UPDATE learnings SET embedding_vector = ?, embedding_status = 'done' WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, doc := range dbDocs {
		if _, err := stmt.ExecContext(ctx, SerializeFloat32(doc.Embedding), doc.ID); err != nil {
			return fmt.Errorf("update %s: %w", doc.ID, err)
		}
	}
	return tx.Commit()
}

// Search finds the most similar documents.
// Delegates to IVF index if active, otherwise brute-force cosine scan.
func (s *VectorStore) Search(ctx context.Context, queryEmbedding []float32, nResults int) ([]SearchResult, error) {
	return s.SearchWithProject(ctx, queryEmbedding, nResults, "")
}

// SearchWithProject finds the most similar documents, filtered by project.
// Empty project means no filter (all projects).
func (s *VectorStore) SearchWithProject(ctx context.Context, queryEmbedding []float32, nResults int, project string) ([]SearchResult, error) {
	s.mu.RLock()
	idx := s.ivfIndex
	s.mu.RUnlock()

	if idx != nil {
		results, err := idx.Search(ctx, queryEmbedding, nResults)
		if err != nil || project == "" {
			return results, err
		}
		// Post-filter IVF results by project. IVF metadata carries only a single
		// "project" field (no canonical_project), so we pass it as both args to
		// projectMatchesTolerant; the both-abs short-circuit prevents /a/foo vs /b/foo
		// collisions while still allowing short-name callers to match full-path metadata.
		filtered := results[:0]
		for _, r := range results {
			rp, _ := r.Metadata["project"]
			if projectMatchesTolerant(rp, rp, project) {
				filtered = append(filtered, r)
			}
		}
		return filtered, nil
	}
	return s.bruteForceScan(ctx, queryEmbedding, nResults, project)
}

// bruteForceScan loads all vectors from SQLite and computes cosine similarity.
func (s *VectorStore) bruteForceScan(ctx context.Context, queryEmbedding []float32, nResults int, project string) ([]SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if nResults <= 0 {
		return nil, nil
	}

	var rows *sql.Rows
	var err error
	// SQL pre-filter to shrink the working set. The post-filter via projectMatchesTolerant
	// below handles edge cases (worktree paths, same-basename collision avoidance) that
	// this clause cannot express in SQL. We pass both the raw requested value and its
	// canonical basename so legacy rows (canonical_project='yesmem') match when the
	// caller passes a full path, and vice versa.
	if project != "" {
		reqCanon := models.CanonicalProject(project)
		rows, err = s.db.QueryContext(ctx,
			`SELECT CAST(id AS TEXT), content, embedding_vector, COALESCE(project, ''), COALESCE(canonical_project, ''), COALESCE(category, '')
			 FROM learnings
			 WHERE embedding_vector IS NOT NULL AND `+models.RecallableLearningSQL+`
			   AND (project = ? OR canonical_project = ? OR project = ? OR canonical_project = ?
			        OR (project = '' AND canonical_project = ''))`,
			project, project, reqCanon, reqCanon)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT CAST(id AS TEXT), content, embedding_vector, COALESCE(project, ''), COALESCE(canonical_project, ''), COALESCE(category, '')
			 FROM learnings
			 WHERE embedding_vector IS NOT NULL AND `+models.RecallableLearningSQL)
	}
	if err != nil {
		return nil, fmt.Errorf("load vectors: %w", err)
	}
	defer rows.Close()

	type scored struct {
		id, content, project, canonicalProject, category string
		similarity                                        float32
	}
	var all []scored

	for rows.Next() {
		var id, content, rowProject, canonicalProject, category string
		var blob []byte
		if err := rows.Scan(&id, &content, &blob, &rowProject, &canonicalProject, &category); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if !projectMatchesTolerant(rowProject, canonicalProject, project) {
			continue
		}
		vec := DeserializeFloat32(blob)
		if len(vec) != len(queryEmbedding) {
			continue
		}
		sim := cosineSimilarity(queryEmbedding, vec)
		all = append(all, scored{id: id, content: content, project: rowProject, canonicalProject: canonicalProject, category: category, similarity: sim})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Also search in-memory AQ docs
	memCount := len(s.memDocs)
	memMatched := 0
	for _, doc := range s.memDocs {
		// Project filter for memDocs (tolerant: short name vs full path vs worktree)
		if project != "" {
			dp, _ := doc.Metadata["project"]
			if dp != "" && !projectMatchesTolerant(dp, "", project) {
				continue
			}
		}
		if len(doc.Embedding) != len(queryEmbedding) {
			continue
		}
		sim := cosineSimilarity(queryEmbedding, doc.Embedding)
		content := doc.Content
		proj := doc.Metadata["project"]
		cat := doc.Metadata["category"]
		// Use learning_content if available (AQ docs carry the parent learning's content)
		if lc, ok := doc.Metadata["learning_content"]; ok && lc != "" {
			content = lc
		}
		lid := doc.Metadata["learning_id"]
		if lid == "" {
			lid = doc.ID
		}
		all = append(all, scored{id: lid, content: content, project: proj, category: cat, similarity: sim})
		memMatched++
	}
	if memCount > 0 {
		fmt.Fprintf(os.Stderr, "    [vec-debug] memDocs=%d matched=%d total_scored=%d\n", memCount, memMatched, len(all))
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].similarity > all[j].similarity
	})

	if nResults > len(all) {
		nResults = len(all)
	}

	results := make([]SearchResult, nResults)
	for i := 0; i < nResults; i++ {
		results[i] = SearchResult{
			ID:         all[i].id,
			Content:    all[i].content,
			Similarity: all[i].similarity,
			Metadata: map[string]string{
				"category": all[i].category,
				"project":  all[i].project,
			},
		}
	}
	return results, nil
}

// SearchAllowed performs a brute-force cosine-similarity search restricted to a set of
// allowed learning IDs. It searches both SQLite learnings (with embedding_vector) and
// in-memory AQ documents. When the allowed set is empty, returns empty results.
func (s *VectorStore) SearchAllowed(ctx context.Context, queryEmbedding []float32, nResults int, allowedIDs map[string]bool) ([]SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if nResults <= 0 || len(allowedIDs) == 0 {
		return nil, nil
	}

	type scored struct {
		id, content, project, category string
		similarity                     float32
	}
	var all []scored

	// One placeholder per allowed ID would bind tens of thousands of variables in a
	// single statement, above SQLite's ceiling of 32766; the date prefilter of
	// hybrid_search routinely reaches that size on a mature corpus.
	ids := make([]string, 0, len(allowedIDs))
	for id := range allowedIDs {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += searchAllowedBatch {
		end := min(start+searchAllowedBatch, len(ids))
		batch := ids[start:end]

		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}

		rows, err := s.db.QueryContext(ctx,
			`SELECT CAST(id AS TEXT), content, embedding_vector, COALESCE(project, ''), COALESCE(category, '')
			 FROM learnings
			 WHERE id IN (`+strings.Join(placeholders, ",")+`)
			 AND embedding_vector IS NOT NULL
			 AND `+models.RecallableLearningSQL, args...)
		if err != nil {
			return nil, fmt.Errorf("load vectors: %w", err)
		}

		for rows.Next() {
			var id, content, project, category string
			var blob []byte
			if err := rows.Scan(&id, &content, &blob, &project, &category); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan: %w", err)
			}
			if !allowedIDs[id] {
				continue
			}
			vec := DeserializeFloat32(blob)
			if len(vec) != len(queryEmbedding) {
				continue
			}
			sim := cosineSimilarity(queryEmbedding, vec)
			all = append(all, scored{id: id, content: content, project: project, category: category, similarity: sim})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	// Also search in-memory AQ docs filtered by allowed IDs
	for _, doc := range s.memDocs {
		lid := doc.Metadata["learning_id"]
		if lid == "" {
			lid = doc.ID
		}
		if !allowedIDs[lid] {
			continue
		}
		if len(doc.Embedding) != len(queryEmbedding) {
			continue
		}
		sim := cosineSimilarity(queryEmbedding, doc.Embedding)
		content := doc.Content
		proj := doc.Metadata["project"]
		cat := doc.Metadata["category"]
		if lc, ok := doc.Metadata["learning_content"]; ok && lc != "" {
			content = lc
		}
		all = append(all, scored{id: lid, content: content, project: proj, category: cat, similarity: sim})
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].similarity > all[j].similarity
	})

	if nResults > len(all) {
		nResults = len(all)
	}

	results := make([]SearchResult, nResults)
	for i := 0; i < nResults; i++ {
		results[i] = SearchResult{
			ID:         all[i].id,
			Content:    all[i].content,
			Similarity: all[i].similarity,
			Metadata: map[string]string{
				"category": all[i].category,
				"project":  all[i].project,
			},
		}
	}
	return results, nil
}

// Delete removes the embedding for a learning ID.
// Delete removes the embedding for a learning ID.
// Also removes from IVF index if active.
func (s *VectorStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE learnings SET embedding_vector = NULL, embedding_status = NULL WHERE id = ?`, id)

	if s.ivfIndex != nil {
		var numID uint64
		fmt.Sscanf(id, "%d", &numID)
		s.ivfIndex.Remove(numID)
	}
	return err
}

// Count returns the number of learnings with embeddings.
func (s *VectorStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM learnings WHERE embedding_vector IS NOT NULL`).Scan(&count)
	return count
}

// ActiveCount returns the number of recallable learnings with embeddings, which
// is exactly the population the IVF index is built from.
func (s *VectorStore) ActiveCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM learnings WHERE embedding_vector IS NOT NULL AND ` + models.RecallableLearningSQL).Scan(&count)
	return count
}

// Has checks if a learning has an embedding.
func (s *VectorStore) Has(ctx context.Context, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM learnings WHERE id = ? AND embedding_vector IS NOT NULL`, id).Scan(&exists)
	return err == nil
}

// GetEmbedding returns the embedding vector for a learning by ID.
func (s *VectorStore) GetEmbedding(ctx context.Context, id string) []float32 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var blob []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT embedding_vector FROM learnings WHERE id = ? AND embedding_vector IS NOT NULL`, id).Scan(&blob)
	if err != nil {
		return nil
	}
	return DeserializeFloat32(blob)
}

// Reload is a no-op — vectors are always read from SQLite.
func (s *VectorStore) Reload() error {
	return nil
}

// Close is a no-op — DB is owned by Storage.
func (s *VectorStore) Close() error {
	return nil
}

// cosineSimilarity computes cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float32 {
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return float32(dot / denom)
}

// SerializeFloat32 converts a float32 slice to little-endian bytes.
func SerializeFloat32(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// DeserializeFloat32 converts little-endian bytes back to a float32 slice.
func DeserializeFloat32(buf []byte) []float32 {
	if len(buf)%4 != 0 {
		return nil
	}
	v := make([]float32, len(buf)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return v
}

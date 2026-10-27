package embedding

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestVectorStoreAddAndSearch(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	ctx := context.Background()

	// Insert learnings rows first (Add updates existing rows)
	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (1, 'nginx reverse proxy', 'test')`)
	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (2, 'web server load balancing', 'test')`)
	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (3, 'chocolate cake recipe', 'test')`)

	docs := []VectorDoc{
		{ID: "1", Embedding: makeVec(384, 0.1)},
		{ID: "2", Embedding: makeVec(384, 0.11)},
		{ID: "3", Embedding: makeVec(384, 0.9)},
	}
	for _, doc := range docs {
		if err := store.Add(ctx, doc); err != nil {
			t.Fatalf("failed to add doc %s: %v", doc.ID, err)
		}
	}

	if store.Count() != 3 {
		t.Fatalf("expected 3 docs, got %d", store.Count())
	}

	results, err := store.Search(ctx, makeVec(384, 0.1), 2)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].ID != "1" {
		t.Errorf("expected 1 as top result, got %s", results[0].ID)
	}
	if results[0].Similarity < 0.99 {
		t.Errorf("expected high similarity for exact match, got %f", results[0].Similarity)
	}
}

func TestVectorStoreDelete(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	ctx := context.Background()
	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (1, 'test', 'test')`)
	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.5)})

	if store.Count() != 1 {
		t.Fatalf("expected 1 doc, got %d", store.Count())
	}

	if err := store.Delete(ctx, "1"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if store.Count() != 0 {
		t.Fatalf("expected 0 docs after delete, got %d", store.Count())
	}
}

func TestVectorStorePersistence(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (1, 'persisted', 'test')`)

	store1, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatal(err)
	}
	store1.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.5)})

	// Same DB — data persists in learnings table
	store2, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatal(err)
	}
	if store2.Count() != 1 {
		t.Fatalf("expected 1 doc after reopen, got %d", store2.Count())
	}
}

func TestVectorStoreAddBatch(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (1, 'one', 'test')`)
	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (2, 'two', 'test')`)
	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (3, 'three', 'test')`)

	docs := []VectorDoc{
		{ID: "1", Embedding: makeVec(384, 0.1)},
		{ID: "2", Embedding: makeVec(384, 0.2)},
		{ID: "3", Embedding: makeVec(384, 0.3)},
	}
	if err := store.AddBatch(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if store.Count() != 3 {
		t.Fatalf("expected 3 docs, got %d", store.Count())
	}
}

func TestVectorStoreHas(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (42, 'test', 'test')`)
	store.Add(ctx, VectorDoc{ID: "42", Embedding: makeVec(384, 0.5)})

	if !store.Has(ctx, "42") {
		t.Error("expected Has(42) = true")
	}
	if store.Has(ctx, "999") {
		t.Error("expected Has(999) = false")
	}
}

func TestVectorStoreGetEmbedding(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	vec := makeVec(384, 0.42)
	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (7, 'test', 'test')`)
	store.Add(ctx, VectorDoc{ID: "7", Embedding: vec})

	got := store.GetEmbedding(ctx, "7")
	if got == nil {
		t.Fatal("expected embedding, got nil")
	}
	if len(got) != 384 {
		t.Fatalf("expected 384 dims, got %d", len(got))
	}
	if got[0] != vec[0] {
		t.Errorf("embedding mismatch: got[0]=%f, want %f", got[0], vec[0])
	}
	if store.GetEmbedding(ctx, "999") != nil {
		t.Error("expected nil for non-existent ID")
	}
}

func TestVectorStorePeriodicSave(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 4)
	if err != nil {
		t.Fatal(err)
	}

	mock := &mockIVFSaver{}
	store.SetIVFIndex(mock)

	path := filepath.Join(t.TempDir(), "periodic.ivf")
	store.SetIVFSavePath(path, 3) // save every 3 adds

	ctx := context.Background()

	// Insert learning rows for Add to work
	for i := 1; i <= 5; i++ {
		db.Exec(`INSERT INTO learnings(id, content, category) VALUES (?, 'test', 'test')`, i)
	}

	// Add 2 vectors — no save yet
	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(4, 0.1)})
	store.Add(ctx, VectorDoc{ID: "2", Embedding: makeVec(4, 0.2)})
	if mock.saveCount != 0 {
		t.Fatalf("expected 0 saves after 2 adds, got %d", mock.saveCount)
	}

	// Add 3rd vector — triggers save
	store.Add(ctx, VectorDoc{ID: "3", Embedding: makeVec(4, 0.3)})
	if mock.saveCount != 1 {
		t.Fatalf("expected 1 save after 3 adds, got %d", mock.saveCount)
	}
	if mock.savePath != path {
		t.Fatalf("expected save to %q, got %q", path, mock.savePath)
	}

	// Counter resets — next 2 adds: no save
	store.Add(ctx, VectorDoc{ID: "4", Embedding: makeVec(4, 0.4)})
	store.Add(ctx, VectorDoc{ID: "5", Embedding: makeVec(4, 0.5)})
	if mock.saveCount != 1 {
		t.Fatalf("expected still 1 save after 5 total adds, got %d", mock.saveCount)
	}
}

func TestVectorStoreSaveIVF(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 4)
	if err != nil {
		t.Fatal(err)
	}

	// No IVF index → SaveIVF should be no-op, no error
	path := filepath.Join(t.TempDir(), "test.ivf")
	if err := store.SaveIVF(path); err != nil {
		t.Fatalf("SaveIVF without index should not error: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("SaveIVF without index should not create a file")
	}

	// Set a mock IVF index and save
	mock := &mockIVFSaver{savePath: ""}
	store.SetIVFIndex(mock)

	if err := store.SaveIVF(path); err != nil {
		t.Fatalf("SaveIVF: %v", err)
	}
	if mock.savePath != path {
		t.Fatalf("expected Save(%q), got Save(%q)", path, mock.savePath)
	}
}

// mockIVFSaver implements IVFSearcher + IVFSaver for testing.
type mockIVFSaver struct {
	savePath  string
	saveCount int
}

func (m *mockIVFSaver) Search(_ context.Context, _ []float32, _ int) ([]SearchResult, error) {
	return nil, nil
}
func (m *mockIVFSaver) Add(_ uint64, _ []float32) {}
func (m *mockIVFSaver) Remove(_ uint64)            {}
func (m *mockIVFSaver) Save(path string) error {
	m.savePath = path
	m.saveCount++
	return nil
}

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if sim := cosineSimilarity(a, b); sim < 0.999 {
		t.Errorf("identical vectors should have similarity ~1.0, got %f", sim)
	}

	c := []float32{0, 1, 0}
	if sim := cosineSimilarity(a, c); sim > 0.001 {
		t.Errorf("orthogonal vectors should have similarity ~0.0, got %f", sim)
	}
}

func TestVectorStoreSearchAllowed(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	ctx := context.Background()
	t0 := time.Now().UTC().Format(time.RFC3339)
	t1 := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)

	// Insert learnings with different timestamps and embeddings
	db.Exec(`INSERT INTO learnings(id, content, category, created_at) VALUES (1, 'recent doc A', 'test', ?)`, t0)
	db.Exec(`INSERT INTO learnings(id, content, category, created_at) VALUES (2, 'old doc B', 'test', ?)`, t1)
	db.Exec(`INSERT INTO learnings(id, content, category, created_at) VALUES (3, 'recent doc C', 'test', ?)`, t0)

	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.1)})
	store.Add(ctx, VectorDoc{ID: "2", Embedding: makeVec(384, 0.11)})
	store.Add(ctx, VectorDoc{ID: "3", Embedding: makeVec(384, 0.9)})

	queryVec := makeVec(384, 0.1)

	// Search with only allowed IDs = {1, 3} — should exclude doc 2
	allowed := map[string]bool{"1": true, "3": true}
	results, err := store.SearchAllowed(ctx, queryVec, 10, allowed)
	if err != nil {
		t.Fatalf("SearchAllowed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.ID == "2" {
			t.Errorf("doc 2 should not be in allowed set")
		}
	}

	// Search with empty allowed set
	results, err = store.SearchAllowed(ctx, queryVec, 10, map[string]bool{})
	if err != nil {
		t.Fatalf("SearchAllowed empty: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for empty allowed set, got %d", len(results))
	}

	// Search with only doc 2 allowed
	results, err = store.SearchAllowed(ctx, queryVec, 10, map[string]bool{"2": true})
	if err != nil {
		t.Fatalf("SearchAllowed single: %v", err)
	}
	if len(results) != 1 || results[0].ID != "2" {
		t.Fatalf("expected only doc 2, got %v", results)
	}
}

// makeVec creates a simple normalized-ish vector for testing.
func makeVec(dims int, seed float32) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = seed + float32(i)*0.001
	}
	var norm float32
	for _, x := range v {
		norm += x * x
	}
	norm = float32(1.0 / float64(norm))
	for i := range v {
		v[i] *= norm
	}
	return v
}

// testDB creates an in-memory SQLite DB with a minimal learnings table.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	db.Exec(`CREATE TABLE IF NOT EXISTS learnings (
		id INTEGER PRIMARY KEY,
		content TEXT NOT NULL DEFAULT '',
		category TEXT NOT NULL DEFAULT '',
		project TEXT NOT NULL DEFAULT '',
		canonical_project TEXT NOT NULL DEFAULT '',
		embedding_vector BLOB,
		embedding_status TEXT,
		superseded_by INTEGER,
		quarantined_at TEXT,
		created_at TEXT NOT NULL DEFAULT ''
	)`)
	return db
}

// TestVectorStoreSearchExcludesQuarantined (YM-44): both scan paths filtered
// superseded_by but not quarantined_at, so quarantined learnings stayed reachable
// through semantic search.
func TestVectorStoreSearchExcludesQuarantined(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	ctx := context.Background()

	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (1, 'active doc', 'test')`)
	db.Exec(`INSERT INTO learnings(id, content, category, quarantined_at) VALUES (2, 'quarantined doc', 'test', '2026-01-01T00:00:00Z')`)
	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.1)})
	store.Add(ctx, VectorDoc{ID: "2", Embedding: makeVec(384, 0.1)})

	full, err := store.Search(ctx, makeVec(384, 0.1), 5)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(full) != 1 || full[0].ID != "1" {
		t.Errorf("brute-force scan returned %v, want only the active doc", full)
	}

	allowed, err := store.SearchAllowed(ctx, makeVec(384, 0.1), 5, map[string]bool{"1": true, "2": true})
	if err != nil {
		t.Fatalf("allowed search failed: %v", err)
	}
	if len(allowed) != 1 || allowed[0].ID != "1" {
		t.Errorf("allowed scan returned %v, want only the active doc", allowed)
	}
}

func TestVectorStoreSearchAllowedLargeSet(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	db.Exec(`INSERT INTO learnings(id, content, category, created_at) VALUES (1, 'doc A', 'test', ?)`, now)
	db.Exec(`INSERT INTO learnings(id, content, category, created_at) VALUES (2, 'doc B', 'test', ?)`, now)
	db.Exec(`INSERT INTO learnings(id, content, category, created_at) VALUES (3, 'doc C', 'test', ?)`, now)
	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.1)})
	store.Add(ctx, VectorDoc{ID: "2", Embedding: makeVec(384, 0.11)})
	store.Add(ctx, VectorDoc{ID: "3", Embedding: makeVec(384, 0.9)})

	// Above maxInClauseVars the full-scan fallback must filter via map instead of IN.
	allowed := map[string]bool{"1": true, "3": true}
	for i := 0; i < 1200; i++ {
		allowed[fmt.Sprintf("ghost-%d", i)] = true
	}

	results, err := store.SearchAllowed(ctx, makeVec(384, 0.1), 10, allowed)
	if err != nil {
		t.Fatalf("SearchAllowed large set: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.ID == "2" {
			t.Errorf("doc 2 must not appear, not in allowed set")
		}
	}
}

// TestVectorStoreSearchWithProject_FullPathAgainstShort verifies the regression fix:
// when a caller passes project="/home/user/memory/yesmem" but learnings carry
// project="yesmem" (legacy) or canonical_project="yesmem", the vector search
// must still return matches. Before the fix bruteForceScan filtered via
// `WHERE project = ?` and excluded everything.
func TestVectorStoreSearchWithProject_FullPathAgainstShort(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	ctx := context.Background()

	db.Exec(`INSERT INTO learnings(id, content, category, project, canonical_project)
		VALUES (1, 'vector project filter regression doc', 'test', 'yesmem', 'yesmem')`)
	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.5)})

	results, err := store.SearchWithProject(ctx, makeVec(384, 0.5), 5, "/home/user/memory/yesmem")
	if err != nil {
		t.Fatalf("SearchWithProject: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected vector hit with full-path project filter against short stored canonical; got 0 (regression)")
	}
}

// TestVectorStoreSearchWithProject_WorktreeAgainstShort covers the worktree case:
// project passed as a .worktrees/ path must match a learning stored with the
// parent repo's short canonical name.
func TestVectorStoreSearchWithProject_WorktreeAgainstShort(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	ctx := context.Background()

	db.Exec(`INSERT INTO learnings(id, content, category, project, canonical_project)
		VALUES (1, 'worktree project filter vector match', 'test', 'yesmem', 'yesmem')`)
	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.5)})

	results, err := store.SearchWithProject(ctx, makeVec(384, 0.5), 5, "/home/user/memory/yesmem/.worktrees/foo")
	if err != nil {
		t.Fatalf("SearchWithProject: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected vector hit with worktree project filter; got 0")
	}
}

// TestVectorStoreSearchWithProject_NoMatchFiltered confirms unrelated projects
// are still excluded (no false positives from tolerance).
func TestVectorStoreSearchWithProject_NoMatchFiltered(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	ctx := context.Background()

	db.Exec(`INSERT INTO learnings(id, content, category, project, canonical_project)
		VALUES (1, 'unrelated project vector must filter out', 'test', 'yesmem', 'yesmem')`)
	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.5)})

	results, err := store.SearchWithProject(ctx, makeVec(384, 0.5), 5, "/home/user/projects/other")
	if err != nil {
		t.Fatalf("SearchWithProject: %v", err)
	}
	for _, r := range results {
		if r.ID == "1" {
			t.Fatalf("expected unrelated project to be filtered, got result from yesmem: %+v", r)
		}
	}
}

// TestVectorStoreSearchWithProject_SameBasenameDifferentParentNotMatch is the collision
// guard for the vector path: /home/a/foo and /home/b/foo share basename "foo" but must
// NOT match each other. Without the both-abs short-circuit in projectMatchesTolerant,
// a caller from /home/b/foo would receive vectors from /home/a/foo.
func TestVectorStoreSearchWithProject_SameBasenameDifferentParentNotMatch(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	ctx := context.Background()

	db.Exec(`INSERT INTO learnings(id, content, category, project, canonical_project)
		VALUES (1, 'same basename vector collision guard', 'test', '/home/user/projects/foo', '/home/user/projects/foo')`)
	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.5)})

	results, err := store.SearchWithProject(ctx, makeVec(384, 0.5), 5, "/home/user/memory/foo")
	if err != nil {
		t.Fatalf("SearchWithProject: %v", err)
	}
	for _, r := range results {
		if r.ID == "1" {
			t.Fatalf("expected /home/user/projects/foo vector NOT to match caller /home/user/memory/foo (same basename, different repo); got result: %+v", r)
		}
	}
}

// TestVectorStoreSearchWithProject_LegacyEmptyProjectMatches verifies that legacy rows
// (empty project/canonical_project) remain discoverable through the vector path.
func TestVectorStoreSearchWithProject_LegacyEmptyProjectMatches(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 384)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	ctx := context.Background()

	db.Exec(`INSERT INTO learnings(id, content, category, project, canonical_project)
		VALUES (1, 'legacy vector row with empty project metadata', 'test', '', '')`)
	store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(384, 0.5)})

	results, err := store.SearchWithProject(ctx, makeVec(384, 0.5), 5, "/home/user/memory/yesmem")
	if err != nil {
		t.Fatalf("SearchWithProject: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected legacy vector row (empty project) to remain discoverable; got 0")
	}
}

package embedding

import (
	"context"
	"strconv"
	"testing"
)

// TestSearchAllowedBatchesIDListLos10c (YM-305 [MEM-F37]): SearchAllowed bound one
// placeholder per allowed ID. The date prefilter of hybrid_search can produce tens
// of thousands of IDs, and SQLite rejects a statement above 32766 variables, so the
// whole vector lane failed with "too many SQL variables".
func TestSearchAllowedBatchesIDListLos10c(t *testing.T) {
	db := testDB(t)
	store, err := NewVectorStore(db, 4)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	ctx := context.Background()

	db.Exec(`INSERT INTO learnings(id, content, category) VALUES (1, 'los10c real vector row', 'test')`)
	if err := store.Add(ctx, VectorDoc{ID: "1", Embedding: makeVec(4, 0.5)}); err != nil {
		t.Fatalf("add doc: %v", err)
	}

	// One real ID plus far more placeholders than SQLite accepts in one statement.
	allowed := map[string]bool{"1": true}
	for i := 0; i < 40000; i++ {
		allowed[strconv.Itoa(1000+i)] = true
	}

	results, err := store.SearchAllowed(ctx, makeVec(4, 0.5), 5, allowed)
	if err != nil {
		t.Fatalf("SearchAllowed with %d allowed IDs: %v", len(allowed), err)
	}
	if len(results) != 1 || results[0].ID != "1" {
		t.Fatalf("expected exactly the one embedded row, got %d results: %+v", len(results), results)
	}
}

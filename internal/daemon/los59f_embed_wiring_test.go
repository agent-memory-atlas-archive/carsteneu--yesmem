package daemon

import (
	"context"
	"sync"
	"testing"

	"github.com/carsteneu/yesmem/internal/embedding"
)

// YM-432: the daemon publishes the vector-search wiring from its async startup
// goroutine (daemon.go) while request paths already read it — SaveIVF on
// shutdown, hybrid_search, vector_search, the docs removal path and the embed
// queue. The probe against the unguarded fields at the base revision cacc07ab
// made the race detector report a DATA RACE on ivfPath and on the provider field
// (there handler_hybrid.go:84 and :114). Every read now goes through the guarded
// wiring.
//
// This test races the publish loop against SaveIVF and the accessors, i.e. the
// ivfPath and provider halves of the set. The queue has its own publish test
// (TestStartEmbedWorkerPublishesQueue): it is written once by StartEmbedWorker
// and read by EmbedLearning, both under the same lock.
func TestEmbedWiringIsRaceFreeAndVisible(t *testing.T) {
	h, s := mustHandlerWithEmbedding(t)
	store, err := embedding.NewVectorStore(s.DB(), 384)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testEmbedProvider{dims: 384}
	indexer := embedding.NewIndexer(provider, store)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			h.SetEmbedding(indexer, store, provider)
			h.SetSearchEmbeddingProvider(provider)
			h.SetIVFPath("")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			h.SaveIVF()
			_ = h.VectorStore()
			_ = h.EmbedProvider()
			_ = h.embedSnapshot().queryProvider()
			h.EmbedLearning(int64(i+1), "content", "pattern", "proj")
		}
	}()
	wg.Wait()

	if h.VectorStore() == nil {
		t.Fatal("vector store not visible after wiring")
	}
	if h.EmbedProvider() == nil {
		t.Fatal("embed provider not visible after wiring")
	}
	if h.embedSnapshot().queryProvider() == nil {
		t.Fatal("query provider not visible after wiring")
	}
}

// The daemon publishes the dedicated query provider before SetEmbedding
// (daemon.go:875 vs :950). A setter that republished the whole wiring set would
// clear it again.
func TestSetEmbeddingKeepsSearchProvider(t *testing.T) {
	h := &Handler{}
	search := &testEmbedProvider{dims: 8}
	index := &testEmbedProvider{dims: 384}

	h.SetSearchEmbeddingProvider(search)
	h.SetEmbedding(nil, nil, index)

	if got := h.embedSnapshot().queryProvider(); got != search {
		t.Fatalf("query provider = %v, want the dedicated search provider", got)
	}
	if got := h.EmbedProvider(); got != index {
		t.Fatalf("embed provider = %v, want the indexing provider", got)
	}
}

// A published IVF path with no store must stay a silent no-op. The positive
// branch (store plus path, which writes a file) is only reached on daemon
// shutdown and has no test.
func TestSaveIVFWithoutStoreIsNoOp(t *testing.T) {
	h := &Handler{}
	h.SetIVFPath("yesmem.ivf")
	h.SaveIVF()
}

// EmbedLearning before StartEmbedWorker must not block or panic (the queue is
// nil until the daemon publishes it).
func TestEmbedLearningWithoutQueueIsNoOp(t *testing.T) {
	h := &Handler{}
	h.EmbedLearning(1, "content", "pattern", "proj")
}

// vector_search without a published wiring answers the same way it did when the
// fields were read directly: an error, not a panic on a nil store.
func TestVectorSearchWithoutWiring(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.Handle(Request{
		Method: "vector_search",
		Params: map[string]any{"query": "anything", "limit": float64(3)},
	})

	if resp.Error != "vector search not enabled" {
		t.Fatalf("error = %q, want %q", resp.Error, "vector search not enabled")
	}
}

// StartEmbedWorker publishes the queue, and EmbedLearning reaches the worker
// afterwards. The worker's job is not asserted here, only that the queue is
// visible to a caller that uses the guard.
func TestStartEmbedWorkerPublishesQueue(t *testing.T) {
	h, s := mustHandlerWithEmbedding(t)
	s.DB().Exec(`INSERT INTO learnings(id, content, category) VALUES (7, 'queued', 'test')`)
	store, err := embedding.NewVectorStore(s.DB(), 384)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testEmbedProvider{dims: 384}
	h.SetEmbedding(embedding.NewIndexer(provider, store), store, provider)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartEmbedWorker(ctx)

	if h.embedSnapshot().queue == nil {
		t.Fatal("StartEmbedWorker did not publish the embed queue")
	}
	h.EmbedLearning(7, "queued", "test", "")
}

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/models"
)

// TestVectorSearchDropsQuarantinedAnticipatedQueryHitLos10b (YM-245 [MEM-F28]):
// vector_search had no metadata gate. Anticipated-query vectors live in memory
// and resolve to their parent learning by learning_id, so they pass neither the
// SQL vector filters nor the hybrid_search metadata block. The quarantined
// parent's content still came back through the RPC.
func TestVectorSearchDropsQuarantinedAnticipatedQueryHitLos10b(t *testing.T) {
	h, s, _ := mustVectorHandler(t)

	quarantinedID := insertAgedLearning(t, s, &models.Learning{
		Content:   "quarantined vector_search sentinel",
		Category:  "gotcha",
		SessionID: "sess-los10b-vec-quar",
		Source:    "llm_extracted",
	})
	activeID := insertAgedLearning(t, s, &models.Learning{
		Content:   "active vector_search sentinel",
		Category:  "gotcha",
		SessionID: "sess-los10b-vec-active",
		Source:    "llm_extracted",
	})
	if _, err := s.QuarantineSession("sess-los10b-vec-quar"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	// The anticipated-query vectors carry their parent's learning_id and content;
	// the store resolves the hit ID from that metadata. Both share the query
	// embedding, so only the quarantine gate separates them.
	provider := &testEmbedProvider{dims: 4}
	queryVec, err := provider.Embed(context.Background(), []string{"los10b probe"})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	docs := make([]embedding.VectorDoc, 0, 2)
	for _, pair := range []struct {
		id      int64
		content string
	}{
		{quarantinedID, "quarantined vector_search sentinel"},
		{activeID, "active vector_search sentinel"},
	} {
		docs = append(docs, embedding.VectorDoc{
			ID:        fmt.Sprintf("%d_aq_0", pair.id),
			Embedding: queryVec[0],
			Metadata: map[string]string{
				"learning_id":      fmt.Sprintf("%d", pair.id),
				"learning_content": pair.content,
				"project":          "",
				"category":         "gotcha",
			},
		})
	}
	if err := h.VectorStore().AddBatch(context.Background(), docs); err != nil {
		t.Fatalf("add anticipated query vectors: %v", err)
	}

	resp := h.Handle(Request{
		Method: "vector_search",
		Params: map[string]any{"query": "los10b probe", "limit": float64(5)},
	})
	if resp.Error != "" {
		t.Fatalf("vector_search error: %s", resp.Error)
	}
	var results []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Result, &results); err != nil {
		t.Fatalf("parse vector_search result: %v", err)
	}

	got := make(map[string]bool, len(results))
	for _, r := range results {
		got[r.ID] = true
	}
	if !got[fmt.Sprintf("%d", activeID)] {
		t.Fatalf("control failed: the active anticipated-query hit is missing: %v", got)
	}
	if got[fmt.Sprintf("%d", quarantinedID)] {
		t.Errorf("quarantined learning %d was returned by vector_search", quarantinedID)
	}
}

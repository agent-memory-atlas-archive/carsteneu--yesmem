package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/models"
)

// YM-273 (3): GetActiveLearnings also returns the global learnings
// (canonical_project = ”), so they became cluster members in every project: the
// same learning showed up in each project's cluster set and counted towards its
// size. Clustering must read the project's own learnings only.
func TestLos25b_ClusterLearningsIgnoresGlobalLearnings(t *testing.T) {
	s := los25EmbeddingTestStore(t)

	const project = "/home/tester/memory/yesmem"
	for i := 0; i < 3; i++ {
		los25UpsertSession(t, s, fmt.Sprintf("los25bsess-%d", i), project)
	}

	vec, err := embedding.NewVectorStore(s.DB(), 4)
	if err != nil {
		t.Fatalf("vector store: %v", err)
	}
	ctx := context.Background()
	add := func(content, learningProject string) int64 {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			Category:   "gotcha",
			Content:    content,
			Project:    learningProject,
			Source:     "llm_extracted",
			Confidence: 1.0,
			CreatedAt:  time.Now().Add(-24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
		if err := vec.Add(ctx, embedding.VectorDoc{ID: strconv.FormatInt(id, 10), Embedding: []float32{1, 0, 0, 0}}); err != nil {
			t.Fatalf("add embedding %d: %v", id, err)
		}
		return id
	}

	projectIDs := make(map[int64]bool, 6)
	for i := 0; i < 6; i++ {
		projectIDs[add(fmt.Sprintf("los25b project learning %d", i), project)] = true
	}
	// Global learnings carry the same embedding, so without the filter they join
	// the cluster and inflate it.
	for i := 0; i < 3; i++ {
		add(fmt.Sprintf("los25b global learning %d", i), "")
	}

	ClusterLearnings(s, &los25CountingLLM{reply: "shared topic"}, vec)

	clusters, err := s.GetLearningClusters(project)
	if err != nil {
		t.Fatalf("get clusters: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(clusters))
	}
	var stored []int64
	if err := json.Unmarshal([]byte(clusters[0].LearningIDs), &stored); err != nil {
		t.Fatalf("parse learning ids: %v", err)
	}
	if len(stored) != len(projectIDs) {
		t.Fatalf("cluster holds %d learnings, want %d (the project's own only)", len(stored), len(projectIDs))
	}
	for _, id := range stored {
		if !projectIDs[id] {
			t.Fatalf("cluster holds learning %d, which belongs to no project", id)
		}
	}
}

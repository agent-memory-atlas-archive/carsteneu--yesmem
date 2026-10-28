package daemon

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/models"
)

func los25InsertDatedLearning(t *testing.T, s interface {
	InsertLearning(*models.Learning) (int64, error)
}, content, project string, createdAt time.Time) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category:   "gotcha",
		Content:    content,
		Project:    project,
		Source:     "llm_extracted",
		Confidence: 1.0,
		CreatedAt:  createdAt,
	})
	if err != nil {
		t.Fatalf("insert learning %q: %v", content, err)
	}
	return id
}

// YM-125: the clustering cap took the learnings with the highest hit_count. The
// tail of a project was therefore never clustered, so distillation and recurrence
// (>=3 members) stayed empty. The cap must cover the whole population.
func TestLos25_CapClusterableCoversTheWholePopulation(t *testing.T) {
	base := time.Now().Add(-1000 * time.Hour)
	learnings := make([]models.Learning, 0, 330)
	for i := 0; i < 330; i++ {
		learnings = append(learnings, models.Learning{
			ID:        int64(i + 1),
			Content:   fmt.Sprintf("learning %d", i+1),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}

	got := capClusterable(learnings, 300)
	if len(got) != 300 {
		t.Fatalf("cap returned %d learnings, want 300", len(got))
	}

	oldest := 0
	for _, l := range got {
		if l.ID <= 30 {
			oldest++
		}
	}
	if oldest < 20 {
		t.Fatalf("only %d of the 30 oldest learnings survived the cap (hit-count head)", oldest)
	}

	// Deterministic: the same input must select the same set, whatever order the
	// database returned it in.
	reversed := make([]models.Learning, len(learnings))
	for i, l := range learnings {
		reversed[len(learnings)-1-i] = l
	}
	second := capClusterable(reversed, 300)
	for i := range got {
		if got[i].ID != second[i].ID {
			t.Fatalf("selection depends on input order: position %d is %d vs %d", i, got[i].ID, second[i].ID)
		}
	}
}

// YM-125 review round 1: an even stride anchored at index 0 never samples the
// newest tail, and the newest learnings are exactly what recurrence looks for.
func TestLos25_CapClusterableKeepsTheNewestLearnings(t *testing.T) {
	base := time.Now().Add(-1000 * time.Hour)
	learnings := make([]models.Learning, 0, 301)
	for i := 0; i < 301; i++ {
		learnings = append(learnings, models.Learning{
			ID:        int64(i + 1),
			Content:   fmt.Sprintf("learning %d", i+1),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}

	got := capClusterable(learnings, 300)
	newest := int64(0)
	for _, l := range got {
		if l.ID > newest {
			newest = l.ID
		}
	}
	if newest != 301 {
		t.Fatalf("newest sampled learning is %d, want the newest one (301)", newest)
	}
}

// YM-125: end to end, a tight cluster among the oldest learnings must still be
// found after the cap. Before the fix the top-300-by-hit-count slice dropped it.
func TestLos25_ClusterLearningsFindsClusterBeyondTheCap(t *testing.T) {
	s := los25EmbeddingTestStore(t)

	const project = "/home/tester/memory/yesmem"
	for i := 0; i < 3; i++ {
		los25UpsertSession(t, s, fmt.Sprintf("los25sess-cap-%d", i), project)
	}

	base := time.Now().Add(-1000 * time.Hour)
	vec, err := embedding.NewVectorStore(s.DB(), 340)
	if err != nil {
		t.Fatalf("vector store: %v", err)
	}
	ctx := context.Background()

	// The oldest 20 learnings share one direction; the 300 newer ones each own a
	// distinct axis, so they never cluster with anything.
	for i := 0; i < 320; i++ {
		id := los25InsertDatedLearning(t, s, fmt.Sprintf("los25 topic %d", i), project, base.Add(time.Duration(i)*time.Minute))
		embeddingVec := make([]float32, 340)
		if i < 20 {
			embeddingVec[0] = 1
		} else {
			embeddingVec[i] = 1
		}
		if err := vec.Add(ctx, embedding.VectorDoc{ID: strconv.FormatInt(id, 10), Embedding: embeddingVec}); err != nil {
			t.Fatalf("add embedding %d: %v", id, err)
		}
	}

	ClusterLearnings(s, &los25CountingLLM{reply: "shared topic"}, vec)

	clusters, err := s.GetLearningClusters("yesmem")
	if err != nil {
		t.Fatalf("get clusters: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1 (the tight group among the oldest learnings)", len(clusters))
	}
	if clusters[0].LearningCount < 5 {
		t.Fatalf("cluster holds %d learnings, want the tight group to survive the cap", clusters[0].LearningCount)
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los25CountingLLM counts completion calls so a test can prove that one cluster
// was labelled once instead of once per worktree copy.
type los25CountingLLM struct {
	mu        sync.Mutex
	calls     int
	lastInput string
	reply     string
}

func (m *los25CountingLLM) Complete(system, user string, opts ...extraction.CallOption) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.lastInput = user
	return m.reply, nil
}

func (m *los25CountingLLM) CompleteJSON(system, user string, schema map[string]any, opts ...extraction.CallOption) (string, error) {
	return m.Complete(system, user, opts...)
}

func (m *los25CountingLLM) Name() string  { return "los25-mock" }
func (m *los25CountingLLM) Model() string { return "los25-mock-model" }

func (m *los25CountingLLM) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func los25UpsertSession(t *testing.T, s *storage.Store, id, project string) {
	t.Helper()
	err := s.UpsertSession(&models.Session{
		ID:           id,
		Project:      project,
		MessageCount: 12,
		StartedAt:    time.Now().Add(-time.Hour),
		IndexedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
}

func los25InsertClusterLearning(t *testing.T, s *storage.Store, content, project string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category:   "gotcha",
		Content:    content,
		Project:    project,
		Source:     "llm_extracted",
		Confidence: 1.0,
		CreatedAt:  time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("insert learning %q: %v", content, err)
	}
	return id
}

func los25EmbeddingTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s := mustStore(t)
	// embedding_vector arrives through a migration, the initial schema lacks it.
	s.DB().Exec(`ALTER TABLE learnings ADD COLUMN embedding_vector BLOB`)
	return s
}

// YM-115: ClusterLearnings looped over session project rows, so every worktree
// path became its own clustering run: one cluster row per path and one LLM label
// call per path. Two worktrees of the same repository must produce one cluster
// set under the canonical project, with one label call per cluster.
func TestLos25_ClusterLearningsRunsOncePerCanonicalProject(t *testing.T) {
	s := los25EmbeddingTestStore(t)

	const wtA = "/home/tester/memory/yesmem/.worktrees/yesloop-los-25"
	const wtB = "/home/tester/memory/yesmem/.worktrees/yesloop-los-31"

	for i := 0; i < 3; i++ {
		los25UpsertSession(t, s, fmt.Sprintf("los25sess-a-%d", i), wtA)
		los25UpsertSession(t, s, fmt.Sprintf("los25sess-b-%d", i), wtB)
	}

	vec, err := embedding.NewVectorStore(s.DB(), 4)
	if err != nil {
		t.Fatalf("vector store: %v", err)
	}

	ctx := context.Background()
	ids := make([]int64, 0, 6)
	for i := 0; i < 6; i++ {
		project := wtA
		if i%2 == 1 {
			project = wtB
		}
		ids = append(ids, los25InsertClusterLearning(t, s, fmt.Sprintf("los25 cluster learning %d", i), project))
	}
	// Two tight similarity groups: ids 0-2 on the first axis, ids 3-5 on the second.
	for i, id := range ids {
		vecValue := []float32{0, 1, 0, 0}
		if i < 3 {
			vecValue = []float32{1, 0, 0, 0}
		}
		if err := vec.Add(ctx, embedding.VectorDoc{ID: strconv.FormatInt(id, 10), Embedding: vecValue}); err != nil {
			t.Fatalf("add embedding %d: %v", id, err)
		}
	}

	client := &los25CountingLLM{reply: "clustering"}
	ClusterLearnings(s, client, vec)

	rows, err := s.DB().Query(`SELECT DISTINCT project FROM learning_clusters`)
	if err != nil {
		t.Fatalf("query cluster projects: %v", err)
	}
	defer rows.Close()
	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan cluster project: %v", err)
		}
		projects = append(projects, p)
	}
	if len(projects) != 1 || projects[0] != "/home/tester/memory/yesmem" {
		t.Fatalf("cluster rows live under %v, want exactly [/home/tester/memory/yesmem]", projects)
	}

	var clusterRows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learning_clusters`).Scan(&clusterRows); err != nil {
		t.Fatalf("count cluster rows: %v", err)
	}
	if clusterRows != 2 {
		t.Fatalf("cluster rows = %d, want 2 (one per similarity group)", clusterRows)
	}
	if got := client.CallCount(); got != 2 {
		t.Fatalf("LLM label calls = %d, want 2 (one per cluster, not one per worktree path)", got)
	}
}

// YM-115: the stored cluster ids must be the learnings the cluster was built
// from, independent of which worktree path wrote them.
func TestLos25_ClusterRowsCarryAllWorktreeLearnings(t *testing.T) {
	s := los25EmbeddingTestStore(t)

	const wtA = "/home/tester/memory/yesmem/.worktrees/yesloop-los-25"
	const wtB = "/home/tester/memory/yesmem/.worktrees/yesloop-los-31"

	for i := 0; i < 3; i++ {
		los25UpsertSession(t, s, fmt.Sprintf("los25sess2-a-%d", i), wtA)
		los25UpsertSession(t, s, fmt.Sprintf("los25sess2-b-%d", i), wtB)
	}

	vec, err := embedding.NewVectorStore(s.DB(), 4)
	if err != nil {
		t.Fatalf("vector store: %v", err)
	}
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 6; i++ {
		project := wtA
		if i%2 == 1 {
			project = wtB
		}
		id := los25InsertClusterLearning(t, s, fmt.Sprintf("los25 shared topic %d", i), project)
		ids = append(ids, id)
		if err := vec.Add(ctx, embedding.VectorDoc{ID: strconv.FormatInt(id, 10), Embedding: []float32{1, 0, 0, 0}}); err != nil {
			t.Fatalf("add embedding %d: %v", id, err)
		}
	}

	ClusterLearnings(s, &los25CountingLLM{reply: "shared topic"}, vec)

	clusters, err := s.GetLearningClusters("yesmem")
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
	if len(stored) != len(ids) {
		t.Fatalf("cluster holds %d learnings, want %d (both worktrees)", len(stored), len(ids))
	}
}

// YM-115 review round 1: recurrence alerts are derived rows. Clustering them
// feeds meta text back into distillation and shifts membership on every alert.
func TestLos25_ClusterLearningsSkipsRecurrenceAlerts(t *testing.T) {
	s := los25EmbeddingTestStore(t)

	const project = "/home/tester/memory/yesmem"
	for i := 0; i < 3; i++ {
		los25UpsertSession(t, s, fmt.Sprintf("los25sess-alert-%d", i), project)
	}

	vec, err := embedding.NewVectorStore(s.DB(), 4)
	if err != nil {
		t.Fatalf("vector store: %v", err)
	}
	ctx := context.Background()
	add := func(category, content string, vecValue []float32) {
		t.Helper()
		id, err := s.InsertLearning(&models.Learning{
			Category:   category,
			Content:    content,
			Project:    project,
			Source:     "llm_extracted",
			Confidence: 1.0,
			CreatedAt:  time.Now().Add(-24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("insert %s: %v", category, err)
		}
		if err := vec.Add(ctx, embedding.VectorDoc{ID: strconv.FormatInt(id, 10), Embedding: vecValue}); err != nil {
			t.Fatalf("add embedding: %v", err)
		}
	}

	for i := 0; i < 6; i++ {
		add("gotcha", fmt.Sprintf("los25 alert-filter gotcha %d", i), []float32{1, 0, 0, 0})
		add("recurrence_alert", fmt.Sprintf("los25 alert-filter alert %d", i), []float32{0, 1, 0, 0})
	}

	ClusterLearnings(s, &los25CountingLLM{reply: "shared topic"}, vec)

	clusters, err := s.GetLearningClusters("yesmem")
	if err != nil {
		t.Fatalf("get clusters: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1 (only the gotcha group; alerts are excluded)", len(clusters))
	}
	if clusters[0].LearningCount != 6 {
		t.Fatalf("cluster holds %d learnings, want 6 gotchas without the alerts", clusters[0].LearningCount)
	}
}

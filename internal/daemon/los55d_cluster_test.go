package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/models"
)

// los55dCheckout creates a repository directory with a .git entry, so the root
// detection resolves it as a checkout instead of treating the path as a bare
// directory.
func los55dCheckout(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("create checkout %s: %v", dir, err)
	}
	return dir
}

func los55dClusterIDs(t *testing.T, raw string) []int64 {
	t.Helper()
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		t.Fatalf("parse cluster learning ids %q: %v", raw, err)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// YM-321 (1): the cluster row set is keyed per repository, but the membership was
// the union over the basename, so two repositories called "repo" put each other's
// learnings into their clusters. The key alone does not separate them: the set of
// learnings has to follow the repository root the learning was written in.
func TestLos55d_ClusterMembershipFollowsTheRepositoryRoot(t *testing.T) {
	s := los25EmbeddingTestStore(t)

	repoA := los55dCheckout(t, filepath.Join(t.TempDir(), "a", "repo"))
	repoB := los55dCheckout(t, filepath.Join(t.TempDir(), "b", "repo"))

	for i := 0; i < 5; i++ {
		los25UpsertSession(t, s, fmt.Sprintf("los55dsess-a-%d", i), repoA)
		los25UpsertSession(t, s, fmt.Sprintf("los55dsess-b-%d", i), repoB)
	}

	vec, err := embedding.NewVectorStore(s.DB(), 4)
	if err != nil {
		t.Fatalf("vector store: %v", err)
	}

	var idsA, idsB []int64
	for i := 0; i < 5; i++ {
		idsA = append(idsA, los25InsertClusterLearning(t, s, fmt.Sprintf("los55d repo A learning %d", i), repoA))
		idsB = append(idsB, los25InsertClusterLearning(t, s, fmt.Sprintf("los55d repo B learning %d", i), repoB))
	}

	// One tight similarity group: every learning lands in one cluster per key, so
	// the only thing the assertion measures is the membership.
	ctx := context.Background()
	for _, id := range append(append([]int64{}, idsA...), idsB...) {
		if err := vec.Add(ctx, embedding.VectorDoc{ID: strconv.FormatInt(id, 10), Embedding: []float32{1, 0, 0, 0}}); err != nil {
			t.Fatalf("add embedding %d: %v", id, err)
		}
	}

	ClusterLearnings(s, nil, vec)

	rows, err := s.DB().Query(`SELECT project, learning_ids FROM learning_clusters`)
	if err != nil {
		t.Fatalf("query cluster rows: %v", err)
	}
	defer rows.Close()
	members := make(map[string][][]int64)
	for rows.Next() {
		var project, raw string
		if err := rows.Scan(&project, &raw); err != nil {
			t.Fatalf("scan cluster row: %v", err)
		}
		members[project] = append(members[project], los55dClusterIDs(t, raw))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cluster rows: %v", err)
	}

	if len(members[repoA]) == 0 || len(members[repoB]) == 0 {
		t.Fatalf("cluster rows live under %d keys, want one per repository (%q and %q)", len(members), repoA, repoB)
	}

	los55dAssertMembers(t, "repo A", members[repoA], idsA, idsB)
	los55dAssertMembers(t, "repo B", members[repoB], idsB, idsA)
}

// A learning whose project is a legacy bare name carries no path, so the membership
// test cannot place it. It must stay in the basename scope it was written under:
// dropping it would silently take the oldest learnings out of every cluster set.
func TestLos55d_LegacyBareNameLearningsStayInTheSet(t *testing.T) {
	s := los25EmbeddingTestStore(t)
	repo := los55dCheckout(t, filepath.Join(t.TempDir(), "repo"))

	for i := 0; i < 3; i++ {
		los25UpsertSession(t, s, fmt.Sprintf("los55dsess-legacy-%d", i), repo)
	}
	ids := make([]int64, 0, 5)
	for i := 0; i < 4; i++ {
		ids = append(ids, los25InsertClusterLearning(t, s, fmt.Sprintf("los55d path learning %d", i), repo))
	}
	legacyID := los25InsertClusterLearning(t, s, "los55d bare-name learning", "repo")
	ids = append(ids, legacyID)

	vec, err := embedding.NewVectorStore(s.DB(), 4)
	if err != nil {
		t.Fatalf("vector store: %v", err)
	}
	ctx := context.Background()
	for _, id := range ids {
		if err := vec.Add(ctx, embedding.VectorDoc{ID: strconv.FormatInt(id, 10), Embedding: []float32{1, 0, 0, 0}}); err != nil {
			t.Fatalf("add embedding %d: %v", id, err)
		}
	}

	ClusterLearnings(s, nil, vec)

	rows, err := s.DB().Query(`SELECT learning_ids FROM learning_clusters`)
	if err != nil {
		t.Fatalf("query cluster rows: %v", err)
	}
	defer rows.Close()
	var members []int64
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan cluster row: %v", err)
		}
		members = append(members, los55dClusterIDs(t, raw)...)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cluster rows: %v", err)
	}
	if len(members) == 0 {
		t.Fatal("no cluster set was built, the bare-name learning was dropped from the membership")
	}
	found := false
	for _, id := range members {
		if id == legacyID {
			found = true
		}
	}
	if !found {
		t.Fatalf("bare-name learning %d is missing from the cluster set (members %v)", legacyID, members)
	}
}

// los55dAssertMembers checks that the clusters under one key hold exactly the
// learnings of that repository and none of the other one's.
func los55dAssertMembers(t *testing.T, label string, clusters [][]int64, own, foreign []int64) {
	t.Helper()
	ownSet := make(map[int64]bool, len(own))
	for _, id := range own {
		ownSet[id] = true
	}
	seenOwn := make(map[int64]bool, len(own))
	for _, ids := range clusters {
		for _, id := range ids {
			if !ownSet[id] {
				t.Errorf("%s: cluster holds learning %d of the same-named other repository (members %v)", label, id, ids)
				continue
			}
			seenOwn[id] = true
		}
	}
	for _, id := range own {
		if !seenOwn[id] {
			t.Errorf("%s: own learning %d is missing from every cluster", label, id)
		}
	}
	for _, id := range foreign {
		if !ownSet[id] && seenOwn[id] {
			t.Errorf("%s: foreign learning %d counted as own", label, id)
		}
	}
}

// A cluster set that was stored before the membership was narrowed may hold
// learnings the repository does not own. When the repository no longer reaches the
// clustering floor, the stored set has to go: keeping it would keep serving foreign
// learnings, and no other run would ever replace it.
func TestLos55d_ClusterFloorClearsTheStoredSet(t *testing.T) {
	s := los25EmbeddingTestStore(t)
	repo := los55dCheckout(t, filepath.Join(t.TempDir(), "repo"))

	for i := 0; i < 3; i++ {
		los25UpsertSession(t, s, fmt.Sprintf("los55dsess-floor-%d", i), repo)
	}
	stale := []models.LearningCluster{{Label: "stale", LearningCount: 3, Confidence: 0.5, LearningIDs: "[1,2,3]"}}
	if err := s.ReplaceLearningClusters(repo, stale); err != nil {
		t.Fatalf("seed stale cluster set: %v", err)
	}
	// Three own learnings, all embedded: below the floor of five.
	ids := []int64{
		los25InsertClusterLearning(t, s, "los55d floor 1", repo),
		los25InsertClusterLearning(t, s, "los55d floor 2", repo),
		los25InsertClusterLearning(t, s, "los55d floor 3", repo),
	}
	vec, err := embedding.NewVectorStore(s.DB(), 4)
	if err != nil {
		t.Fatalf("vector store: %v", err)
	}
	ctx := context.Background()
	for _, id := range ids {
		if err := vec.Add(ctx, embedding.VectorDoc{ID: strconv.FormatInt(id, 10), Embedding: []float32{1, 0, 0, 0}}); err != nil {
			t.Fatalf("add embedding %d: %v", id, err)
		}
	}

	ClusterLearnings(s, nil, vec)

	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learning_clusters`).Scan(&rows); err != nil {
		t.Fatalf("count cluster rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("cluster rows = %d, want none below the clustering floor", rows)
	}
}

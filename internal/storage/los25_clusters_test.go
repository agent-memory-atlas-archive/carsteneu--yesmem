package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-115: clustering ran once per worktree path, so one cluster existed once per
// worktree. Cluster rows must live under the canonical project key, so a write
// from inside a worktree and a read from the main repo name the same row.
func TestLos25_ClusterRowsAreKeyedByCanonicalProject(t *testing.T) {
	s := mustOpen(t)

	const worktree = "/home/tester/memory/yesmem/.worktrees/yesloop-los-25"
	const mainRepo = "/home/tester/memory/yesmem"

	cluster := models.LearningCluster{
		Label:         "clustering",
		LearningCount: 3,
		Confidence:    0.8,
		LearningIDs:   "[1,2,3]",
	}
	if err := s.ReplaceLearningClusters(worktree, []models.LearningCluster{cluster}); err != nil {
		t.Fatalf("replace clusters from worktree path: %v", err)
	}

	got, err := s.GetLearningClusters(mainRepo)
	if err != nil {
		t.Fatalf("get clusters for main repo: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("main repo sees %d cluster rows, want 1 (rows are keyed per worktree path)", len(got))
	}
	if got[0].Project != mainRepo {
		t.Fatalf("cluster key = %q, want repository root %q", got[0].Project, mainRepo)
	}

	// The second writer must replace the same row, not add a second copy.
	if err := s.ReplaceLearningClusters(mainRepo, []models.LearningCluster{cluster}); err != nil {
		t.Fatalf("replace clusters from main repo path: %v", err)
	}
	all, err := s.GetLearningClusters(worktree)
	if err != nil {
		t.Fatalf("get clusters for worktree: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("worktree sees %d cluster rows after two writes, want 1", len(all))
	}
}

// YM-115: the fingerprint must cover the row set clustering reads, so worktree
// learnings invalidate an unchanged-looking project. YM-273 flipped the second
// half of this test: global learnings are no longer part of that row set, so they
// must not invalidate it (see TestLos25b_ClusterFingerprintIgnoresGlobalLearnings).
func TestLos25_ClusterChangeFingerprintCoversWorktreeLearnings(t *testing.T) {
	s := mustOpen(t)

	const worktree = "/home/tester/memory/yesmem/.worktrees/yesloop-los-25"
	insert := func(content, project, category string) {
		t.Helper()
		if _, err := s.InsertLearning(&models.Learning{
			Category:  category,
			Content:   content,
			Project:   project,
			Source:    "llm_extracted",
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
	}
	fingerprint := func() string {
		t.Helper()
		fp, err := s.ClusterChangeFingerprint("yesmem")
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		return fp
	}

	insert("los25 first worktree learning", worktree, "gotcha")
	before := fingerprint()
	if before == "" {
		t.Fatal("empty fingerprint")
	}
	if again := fingerprint(); again != before {
		t.Fatalf("fingerprint not stable: %q then %q", before, again)
	}

	insert("los25 second worktree learning", worktree, "gotcha")
	after := fingerprint()
	if after == before {
		t.Fatal("a new worktree learning did not change the fingerprint")
	}

	insert("los25 global learning", "", "gotcha")
	afterGlobal := fingerprint()
	insert("los25 another global learning", "", "gotcha")
	if final := fingerprint(); final != afterGlobal {
		t.Fatal("a global learning changed the fingerprint, though clustering skips it")
	}

	// Rows clustering drops must not invalidate its change detector.
	stable := fingerprint()
	insert("los25 narrative row", worktree, "narrative")
	insert("los25 unfinished row", worktree, "unfinished")
	insert("los25 strategic row", worktree, "strategic")
	insert("los25 recurrence alert row", worktree, "recurrence_alert")
	if got := fingerprint(); got != stable {
		t.Fatal("a row clustering never reads changed the fingerprint")
	}
}

// YM-115: the reader must merge worktree copies of one project into a single
// group with the session counts summed, otherwise every reader still pays for
// one pass per path.
func TestLos25_GroupByCanonicalProjectMergesWorktrees(t *testing.T) {
	projects := []ProjectSummary{
		{Project: "/home/tester/memory/yesmem", ProjectShort: "/home/tester/memory/yesmem", SessionCount: 7},
		{Project: "/home/tester/memory/yesmem/.worktrees/yesloop-los-25", ProjectShort: "/home/tester/memory/yesmem/.worktrees/yesloop-los-25", SessionCount: 4},
		{Project: "/home/tester/memory/yesmem/.worktrees/yesloop-los-31", ProjectShort: "/home/tester/memory/yesmem/.worktrees/yesloop-los-31", SessionCount: 2},
		{Project: "/home/tester/other", ProjectShort: "/home/tester/other", SessionCount: 5},
	}

	groups := GroupByCanonicalProject(projects)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (yesmem + other): %+v", len(groups), groups)
	}
	byKey := make(map[string]int, len(groups))
	for _, g := range groups {
		byKey[g.Canonical] = g.Sessions
	}
	if byKey["yesmem"] != 13 {
		t.Fatalf("yesmem sessions = %d, want 13 (7+4+2)", byKey["yesmem"])
	}
	if byKey["other"] != 5 {
		t.Fatalf("other sessions = %d, want 5", byKey["other"])
	}
}

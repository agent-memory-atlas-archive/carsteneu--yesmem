package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

func los25bCluster(label string) models.LearningCluster {
	return models.LearningCluster{
		Label:         label,
		LearningCount: 3,
		Confidence:    0.8,
		LearningIDs:   "[1,2,3]",
	}
}

// YM-273 (2): canonical keys were basenames, so two repositories that share a
// name shared one cluster set: the second writer replaced the first one's rows and
// every reader saw whichever repository wrote last.
func TestLos25b_ClusterRowsDoNotMixSameBasenameRepos(t *testing.T) {
	s := mustOpen(t)

	const repoA = "/home/tester/memory/yesmem"
	const repoB = "/home/tester/archive/yesmem"
	const worktreeA = repoA + "/.worktrees/yesloop-los-25b"

	if err := s.ReplaceLearningClusters(repoA, []models.LearningCluster{los25bCluster("a")}); err != nil {
		t.Fatalf("replace clusters for repo A: %v", err)
	}

	gotB, err := s.GetLearningClusters(repoB)
	if err != nil {
		t.Fatalf("get clusters for repo B: %v", err)
	}
	if len(gotB) != 0 {
		t.Fatalf("repo B sees %d cluster rows of repo A (same basename = same key)", len(gotB))
	}

	// The worktree of repo A must still share repo A's set.
	gotWT, err := s.GetLearningClusters(worktreeA)
	if err != nil {
		t.Fatalf("get clusters for worktree of repo A: %v", err)
	}
	if len(gotWT) != 1 || gotWT[0].Label != "a" {
		t.Fatalf("worktree of repo A sees %+v, want the single repo A cluster", gotWT)
	}

	if err := s.ReplaceLearningClusters(repoB, []models.LearningCluster{los25bCluster("b")}); err != nil {
		t.Fatalf("replace clusters for repo B: %v", err)
	}
	gotA, err := s.GetLearningClusters(repoA)
	if err != nil {
		t.Fatalf("get clusters for repo A after repo B wrote: %v", err)
	}
	if len(gotA) != 1 || gotA[0].Label != "a" {
		t.Fatalf("repo A sees %+v after repo B wrote its own set, want its own cluster", gotA)
	}
}

// YM-273 (2): a caller that only knows a bare project name (legacy short name)
// still has to find the repository's clusters, but never another repository's set
// when two repositories share that name.
func TestLos25b_BareProjectNameResolvesToTheOnlyMatchingRepository(t *testing.T) {
	s := mustOpen(t)

	const repoA = "/home/tester/memory/yesmem"
	const repoB = "/home/tester/archive/yesmem"

	if err := s.ReplaceLearningClusters(repoA, []models.LearningCluster{los25bCluster("a")}); err != nil {
		t.Fatalf("replace clusters for repo A: %v", err)
	}
	got, err := s.GetLearningClusters("yesmem")
	if err != nil {
		t.Fatalf("get clusters by bare name: %v", err)
	}
	if len(got) != 1 || got[0].Label != "a" {
		t.Fatalf("bare name sees %+v, want the single repository's cluster", got)
	}

	if err := s.ReplaceLearningClusters(repoB, []models.LearningCluster{los25bCluster("b")}); err != nil {
		t.Fatalf("replace clusters for repo B: %v", err)
	}
	ambiguous, err := s.GetLearningClusters("yesmem")
	if err != nil {
		t.Fatalf("get clusters by ambiguous bare name: %v", err)
	}
	if len(ambiguous) != 0 {
		t.Fatalf("bare name sees %d rows while two repositories carry it, want 0", len(ambiguous))
	}
}

// YM-273 (2): the grouping must produce one group per repository, so two
// repositories that share a basename no longer share a cluster set.
func TestLos25b_GroupByCanonicalProjectSeparatesSameBasenameRepos(t *testing.T) {
	projects := []ProjectSummary{
		{Project: "/home/tester/memory/yesmem", ProjectShort: "/home/tester/memory/yesmem", SessionCount: 7},
		{Project: "/home/tester/memory/yesmem/.worktrees/yesloop-los-25b", ProjectShort: "/home/tester/memory/yesmem/.worktrees/yesloop-los-25b", SessionCount: 4},
		{Project: "/home/tester/archive/yesmem", ProjectShort: "/home/tester/archive/yesmem", SessionCount: 5},
	}

	groups := GroupByCanonicalProject(projects)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (memory/yesmem + archive/yesmem): %+v", len(groups), groups)
	}
	perKey := make(map[string]int, len(groups))
	for _, g := range groups {
		if g.Canonical != "yesmem" {
			t.Fatalf("group %q carries canonical %q, want the learnings scope %q", g.ClusterKey, g.Canonical, "yesmem")
		}
		perKey[g.ClusterKey] = g.Sessions
	}
	if perKey["/home/tester/memory/yesmem"] != 11 {
		t.Fatalf("memory/yesmem sessions = %d, want 11 (7 main + 4 worktree): %+v", perKey["/home/tester/memory/yesmem"], perKey)
	}
	if perKey["/home/tester/archive/yesmem"] != 5 {
		t.Fatalf("archive/yesmem sessions = %d, want 5: %+v", perKey["/home/tester/archive/yesmem"], perKey)
	}
}

// YM-273 (2): a legacy row that names its repository by basename only describes
// one repository too, so it joins the single root that carries the name instead of
// opening a second cluster set.
func TestLos25b_BareProjectNameJoinsTheOnlyMatchingRepository(t *testing.T) {
	projects := []ProjectSummary{
		{Project: "/home/tester/memory/yesmem", ProjectShort: "/home/tester/memory/yesmem", SessionCount: 7},
		{Project: "yesmem", ProjectShort: "yesmem", SessionCount: 2},
	}

	groups := GroupByCanonicalProject(projects)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1 (the bare row belongs to the only yesmem root): %+v", len(groups), groups)
	}
	if groups[0].ClusterKey != "/home/tester/memory/yesmem" || groups[0].Sessions != 9 {
		t.Fatalf("group = %+v, want key /home/tester/memory/yesmem with 9 sessions", groups[0])
	}
}

// YM-273 (3): global learnings (canonical_project = ”) belong to no repository,
// so they must not invalidate every project's change detector. Otherwise one
// global learning re-runs clustering for every project on the installation.
func TestLos25b_ClusterFingerprintIgnoresGlobalLearnings(t *testing.T) {
	s := mustOpen(t)

	const repo = "/home/tester/memory/yesmem"
	insert := func(content, project string) {
		t.Helper()
		if _, err := s.InsertLearning(&models.Learning{
			Category:  "gotcha",
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
		fp, err := s.ClusterChangeFingerprint(repo)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		return fp
	}

	insert("los25b repo learning", repo)
	before := fingerprint()
	if before == "" {
		t.Fatal("empty fingerprint")
	}

	insert("los25b global learning", "")
	if got := fingerprint(); got != before {
		t.Fatalf("a global learning changed the project fingerprint (%q -> %q), though clustering no longer reads it", before, got)
	}

	// A learning of the project itself still has to invalidate the detector.
	insert("los25b second repo learning", repo)
	if got := fingerprint(); got == before {
		t.Fatal("a new project learning did not change the fingerprint")
	}
}

// YM-273 (1): cluster rows keyed by a worktree path or by a bare basename became
// unreachable when the key space moved, and nothing ever deleted them:
// ReplaceLearningClusters only touches the key it writes. The cleanup must report
// them first and delete them only on confirmation.
func TestLos25b_OrphanClusterRowsAreReportedBeforeDeletion(t *testing.T) {
	s := mustOpen(t)

	const repo = "/home/tester/memory/yesmem"
	if err := s.UpsertSession(&models.Session{ID: "los25b-live-session", Project: repo, StartedAt: time.Now()}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	// One live row under the current key, written the normal way.
	if err := s.ReplaceLearningClusters(repo, []models.LearningCluster{los25bCluster("live")}); err != nil {
		t.Fatalf("replace live clusters: %v", err)
	}
	// Two rows from the old key spaces: the basename and a worktree path.
	for _, legacy := range []string{"yesmem", repo + "/.worktrees/yesloop-los-25"} {
		if _, err := s.DB().Exec(`INSERT INTO learning_clusters (project, label, learning_count, learning_ids)
			VALUES (?, 'legacy', 3, '[1,2,3]')`, legacy); err != nil {
			t.Fatalf("insert legacy cluster row %q: %v", legacy, err)
		}
	}

	res, err := s.PruneOrphanLearningClusters(false)
	found, deleted := res.Orphans, res.Deleted
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("dry run deleted %d rows, want 0", deleted)
	}
	if len(found) != 2 {
		t.Fatalf("dry run found %v, want the two legacy keys", found)
	}
	var remaining int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learning_clusters`).Scan(&remaining); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("%d rows after the dry run, want 3 (nothing deleted)", remaining)
	}

	res, err = s.PruneOrphanLearningClusters(true)
	found, deleted = res.Orphans, res.Deleted
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(found) != 2 || deleted != 2 {
		t.Fatalf("apply found %v and deleted %d, want the two legacy keys deleted", found, deleted)
	}

	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learning_clusters`).Scan(&remaining); err != nil {
		t.Fatalf("count rows after apply: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("%d rows after apply, want 1 (the live cluster only)", remaining)
	}
	clusters, err := s.GetLearningClusters(repo)
	if err != nil {
		t.Fatalf("get live clusters: %v", err)
	}
	if len(clusters) != 1 || clusters[0].Label != "live" {
		t.Fatalf("live clusters = %+v, want the untouched live row", clusters)
	}
}

// YM-273 review (cold review, Important 1): an empty live set means the session
// table could not name a single project, not that every cluster is orphaned. The
// sweep must no-op instead of emptying the table on one --apply.
func TestLos25b_OrphanPruneIsNoOpWithoutLiveProjects(t *testing.T) {
	s := mustOpen(t)

	const repo = "/home/tester/memory/yesmem"
	// No sessions at all, so no project is live; the cluster row is still the only
	// state there is.
	if err := s.ReplaceLearningClusters(repo, []models.LearningCluster{los25bCluster("orphan")}); err != nil {
		t.Fatalf("replace clusters: %v", err)
	}

	res, err := s.PruneOrphanLearningClusters(true)
	found, deleted := res.Orphans, res.Deleted
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(found) != 0 || deleted != 0 {
		t.Fatalf("prune found %v and deleted %d rows with no live project, want a no-op", found, deleted)
	}
	var remaining int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learning_clusters`).Scan(&remaining); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("%d cluster rows left, want the row kept (empty live set is not evidence)", remaining)
	}
}

// YM-273 review (security review, NEW-2): a deleted key must not keep its
// change-detection hash. A surviving hash would make the next clustering run
// believe the project is up to date and the removed rows would never come back.
func TestLos25b_OrphanPruneDropsTheClusterHash(t *testing.T) {
	s := mustOpen(t)

	const live = "/home/tester/memory/yesmem"
	// The old key space used basenames; such a key is its own root and stays
	// unreachable, which is exactly what the sweep has to collect.
	const legacy = "yesmem"
	if err := s.UpsertSession(&models.Session{ID: "los25b-hash-session", Project: live, StartedAt: time.Now()}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := s.ReplaceLearningClusters(live, []models.LearningCluster{los25bCluster("live")}); err != nil {
		t.Fatalf("replace live clusters: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO learning_clusters (project, label, learning_count, learning_ids)
		VALUES (?, 'legacy', 3, '[1,2,3]')`, legacy); err != nil {
		t.Fatalf("insert legacy cluster row: %v", err)
	}
	// Written as raw proxy state: SetClusterHash refuses a keyless project (YM-424), but
	// the hash of a key that was written before that guard is exactly what the sweep has
	// to collect.
	if err := s.SetProxyState("cluster_hash:"+legacy, "stale-hash"); err != nil {
		t.Fatalf("set legacy hash: %v", err)
	}
	if err := s.SetClusterHash(live, "live-hash"); err != nil {
		t.Fatalf("set live hash: %v", err)
	}

	if _, err := s.PruneOrphanLearningClusters(true); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if got, _ := s.GetProxyState("cluster_hash:" + legacy); got != "" {
		t.Fatalf("orphan key kept its cluster hash %q, so the project would never be clustered again", got)
	}
	if got, _ := s.GetClusterHash(live); got != "live-hash" {
		t.Fatalf("live project's cluster hash = %q, want it untouched", got)
	}
}

// YM-273 review (consequence review, F3): MigrateProject renamed cluster rows by
// short name. Cluster rows are keyed by repository root, so a rename has to move
// the root key as well.
func TestLos25b_MigrateProjectMovesClusterRows(t *testing.T) {
	s := mustOpen(t)

	const fromPath = "/home/tester/memory/yesmem"
	const toPath = "/home/tester/memory/yesmem-next"
	if err := s.ReplaceLearningClusters(fromPath, []models.LearningCluster{los25bCluster("moved")}); err != nil {
		t.Fatalf("replace clusters: %v", err)
	}

	if _, err := s.MigrateProject(fromPath, toPath, "yesmem", "yesmem-next"); err != nil {
		t.Fatalf("migrate project: %v", err)
	}

	moved, err := s.GetLearningClusters(toPath)
	if err != nil {
		t.Fatalf("get clusters for new path: %v", err)
	}
	if len(moved) != 1 || moved[0].Label != "moved" {
		t.Fatalf("new path sees %+v, want the moved cluster", moved)
	}
	if stale, err := s.GetLearningClusters(fromPath); err != nil {
		t.Fatalf("get clusters for old path: %v", err)
	} else if len(stale) != 0 {
		t.Fatalf("old path still sees %d cluster rows after the rename", len(stale))
	}
}

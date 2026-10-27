package storage

import (
	"sort"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

func los55dSeedSession(t *testing.T, s *Store, id, project string) {
	t.Helper()
	if err := s.UpsertSession(&models.Session{ID: id, Project: project}); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

// los55dProxyKeys lists the proxy_state keys that carry the given prefix.
func los55dProxyKeys(t *testing.T, s *Store, prefix string) []string {
	t.Helper()
	rows, err := s.proxyStateDB().Query(
		`SELECT key FROM proxy_state WHERE substr(key, 1, ?) = ? ORDER BY key`, len(prefix), prefix)
	if err != nil {
		t.Fatalf("list %s keys: %v", prefix, err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan %s key: %v", k, err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s keys: %v", prefix, err)
	}
	return keys
}

// YM-321 (3): the cluster key space moved from basenames and worktree paths to
// repository roots in YM-273, and the change-detection hashes stayed behind. The
// row sweep only ever removed the hash of a key that still had cluster rows, so a
// hash whose rows were already gone survived forever and could make a returning
// project believe its clusters are current.
func TestLos55d_ClusterHashSweepDropsDeadKeys(t *testing.T) {
	s := mustOpenFiles(t)

	liveRoot := "/home/testuser/projects/live"
	los55dSeedSession(t, s, "los55dsess-live", liveRoot)

	// A live key with a hash: has to survive.
	if err := s.SetClusterHash(liveRoot, "hash-live"); err != nil {
		t.Fatalf("set live hash: %v", err)
	}
	// A legacy basename key with no sessions and no rows: dead.
	if err := s.SetProxyState("cluster_hash:live", "hash-old-basename"); err != nil {
		t.Fatalf("set old basename hash: %v", err)
	}
	// A legacy worktree-path key with no rows: dead.
	if err := s.SetProxyState("cluster_hash:"+liveRoot+"/.worktrees/yesloop-los-25b", "hash-old-worktree"); err != nil {
		t.Fatalf("set old worktree hash: %v", err)
	}
	// A key that still owns cluster rows but has no live project: the row sweep
	// owns it, so its hash stays until the rows are gone.
	staleRowsKey := "/home/testuser/projects/stale"
	if err := s.ReplaceLearningClusters(staleRowsKey, []models.LearningCluster{los25bCluster("stale")}); err != nil {
		t.Fatalf("seed stale clusters: %v", err)
	}
	if err := s.SetClusterHash(staleRowsKey, "hash-stale-rows"); err != nil {
		t.Fatalf("set stale hash: %v", err)
	}

	dead, deleted, err := s.PruneOrphanClusterHashes(false)
	if err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	sort.Strings(dead)
	if len(dead) != 2 || dead[0] != "cluster_hash:"+liveRoot+"/.worktrees/yesloop-los-25b" || dead[1] != "cluster_hash:live" {
		t.Fatalf("dry-run reported %v, want the two dead legacy hashes", dead)
	}
	if deleted != 0 {
		t.Fatalf("dry-run deleted %d keys, want 0", deleted)
	}
	if got := los55dProxyKeys(t, s, "cluster_hash:"); len(got) != 4 {
		t.Fatalf("dry-run changed the key set: %v", got)
	}

	dead, deleted, err = s.PruneOrphanClusterHashes(true)
	if err != nil {
		t.Fatalf("apply sweep: %v", err)
	}
	if len(dead) != 2 || deleted != 2 {
		t.Fatalf("apply reported %d keys and deleted %d, want 2 and 2", len(dead), deleted)
	}
	got := los55dProxyKeys(t, s, "cluster_hash:")
	sort.Strings(got)
	want := []string{"cluster_hash:" + liveRoot, "cluster_hash:" + staleRowsKey}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("surviving hashes = %v, want %v", got, want)
	}

	// The row sweep already drops the hash of every key whose rows it deletes, so
	// the two sweeps compose without leaving a hash behind.
	if _, err := s.PruneOrphanLearningClusters(true); err != nil {
		t.Fatalf("row sweep: %v", err)
	}
	if got := los55dProxyKeys(t, s, "cluster_hash:"); len(got) != 1 || got[0] != "cluster_hash:"+liveRoot {
		t.Fatalf("after the row sweep the remaining hashes are %v, want only the live key", got)
	}
}

// A store with no live project at all says nothing about which hashes are dead, so
// the sweep must leave everything alone instead of emptying the key space.
func TestLos55d_ClusterHashSweepIsNoOpWithoutLiveProjects(t *testing.T) {
	s := mustOpenFiles(t)
	if err := s.SetProxyState("cluster_hash:anything", "hash-1"); err != nil {
		t.Fatalf("set hash: %v", err)
	}

	dead, deleted, err := s.PruneOrphanClusterHashes(true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(dead) != 0 || deleted != 0 {
		t.Fatalf("sweep reported %v and deleted %d without any live project", dead, deleted)
	}
	if got := los55dProxyKeys(t, s, "cluster_hash:"); len(got) != 1 {
		t.Fatalf("sweep dropped hashes of an empty project set: %v", got)
	}
}

// A key that still carries cluster rows belongs to the row sweep: it removes the
// rows and the hash together. The hash sweep must not take the change detection away
// from rows that are still being served, and must not double-report them.
func TestLos55d_ClusterHashSweepLeavesKeysWithRowsAlone(t *testing.T) {
	s := mustOpenFiles(t)
	const liveRoot = "/home/testuser/projects/live"
	const withRows = "/home/testuser/projects/stale"
	los55dSeedSession(t, s, "los55dsess-live", liveRoot)

	if err := s.ReplaceLearningClusters(withRows, []models.LearningCluster{los25bCluster("stale")}); err != nil {
		t.Fatalf("seed stale clusters: %v", err)
	}
	if err := s.SetClusterHash(withRows, "hash-rows"); err != nil {
		t.Fatalf("set hash: %v", err)
	}
	if err := s.SetProxyState("cluster_hash:live", "hash-old-basename"); err != nil {
		t.Fatalf("set old hash: %v", err)
	}

	dead, deleted, err := s.PruneOrphanClusterHashes(true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(dead) != 1 || dead[0] != "cluster_hash:live" || deleted != 1 {
		t.Fatalf("sweep reported %v and deleted %d, want only the basename key", dead, deleted)
	}
	if got := los55dProxyKeys(t, s, "cluster_hash:"); len(got) != 1 || got[0] != "cluster_hash:"+withRows {
		t.Fatalf("surviving hashes = %v, want the key that still carries rows", got)
	}
}

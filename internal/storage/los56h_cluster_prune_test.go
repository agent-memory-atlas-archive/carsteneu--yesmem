package storage

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// los56hPruneStore seeds one orphan cluster key beside one live project and returns the
// store plus the orphan key.
func los56hPruneStore(t *testing.T) (*Store, string) {
	t.Helper()
	s := mustOpenFiles(t)
	live := "/home/testuser/projects/prune-live"
	if err := s.UpsertSession(&models.Session{ID: "los56h-prune-live", Project: live}); err != nil {
		t.Fatalf("seed live session: %v", err)
	}
	orphan := "/home/testuser/projects/prune-orphan"
	if err := s.ReplaceLearningClusters(orphan, []models.LearningCluster{
		{Label: "orphan cluster", LearningCount: 3, LearningIDs: "[1,2,3]"},
	}); err != nil {
		t.Fatalf("seed orphan cluster: %v", err)
	}
	if err := s.SetClusterHash(orphan, "orphan-hash"); err != nil {
		t.Fatalf("seed orphan hash: %v", err)
	}
	return s, orphan
}

// TestLos56hPruneRemovesTheHashBeforeTheRowsCommit covers YM-390(4): the row sweep
// committed the cluster rows and deleted the change-detection hashes afterwards. A
// failure between the two left a hash for a key whose rows were already gone, and a key
// that comes back then believes its clusters are current and never rebuilds them. The
// hash has to be gone before the rows commit, so the surviving state is the harmless one.
func TestLos56hPruneRemovesTheHashBeforeTheRowsCommit(t *testing.T) {
	s, orphan := los56hPruneStore(t)
	if value, err := s.GetProxyState("cluster_hash:" + orphan); err != nil || value == "" {
		t.Fatalf("the fixture must carry a hash before the sweep, got %q (err %v)", value, err)
	}

	var hashAtCommit string
	hookRan := false
	previous := pruneClustersPreCommitHook.Load()
	hook := func() {
		hookRan = true
		// runtime.db is a second file: reading it here does not wait on the write lock
		// the row sweep holds.
		if value, err := s.GetProxyState("cluster_hash:" + orphan); err == nil {
			hashAtCommit = value
		}
	}
	pruneClustersPreCommitHook.Store(&hook)
	t.Cleanup(func() { pruneClustersPreCommitHook.Store(previous) })

	res, err := s.PruneOrphanLearningClusters(true)
	orphans, deleted := res.Orphans, res.Deleted
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(orphans) != 1 || deleted == 0 {
		t.Fatalf("the orphan key must be swept, got keys=%v deleted=%d", orphans, deleted)
	}
	if !hookRan {
		t.Fatal("the transaction hook did not run: the order of the two steps is unproven")
	}
	if hashAtCommit != "" {
		t.Errorf("the change-detection hash %q is still there when the rows commit: a failure between the two leaves a key that believes it is current", hashAtCommit)
	}
}

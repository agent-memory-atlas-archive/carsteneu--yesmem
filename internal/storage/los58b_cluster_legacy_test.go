package storage

import (
	"fmt"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los58bLearning inserts one active learning for project and returns its ID.
func los58bLearning(t *testing.T, s *Store, project, content string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		Category:  "gotcha",
		Content:   content,
		Project:   project,
		Source:    "llm_extracted",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert learning %q: %v", content, err)
	}
	return id
}

// los58bClusterRow inserts a cluster row under an arbitrary key, bypassing the writer:
// the old key spaces (bare names, empty) can no longer be produced by the current code,
// which is exactly why they have to be handled on the read and sweep side.
func los58bClusterRow(t *testing.T, s *Store, key, label string, ids ...int64) {
	t.Helper()
	raw := "["
	for i, id := range ids {
		if i > 0 {
			raw += ","
		}
		raw += fmt.Sprint(id)
	}
	raw += "]"
	if _, err := s.DB().Exec(`INSERT INTO learning_clusters (project, label, learning_count, learning_ids)
		VALUES (?, ?, ?, ?)`, key, label, len(ids), raw); err != nil {
		t.Fatalf("insert cluster row %q: %v", key, err)
	}
}

func los58bClusterCount(t *testing.T, s *Store, key string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM learning_clusters WHERE project = ?`, key).Scan(&n); err != nil {
		t.Fatalf("count rows for %q: %v", key, err)
	}
	return n
}

// YM-424: a bare name and an empty value name no repository, so writing a cluster set
// under them is a bug, not a fallback. The live database still carries two rows under
// an empty key from before this was prevented.
func TestLos58bClusterWriterRefusesAKeylessProject(t *testing.T) {
	s := mustOpen(t)

	for _, project := range []string{"", "yesmem"} {
		if err := s.ReplaceLearningClusters(project, []models.LearningCluster{los25bCluster("x")}); err == nil {
			t.Errorf("ReplaceLearningClusters(%q) succeeded, want an error: it has no repository key", project)
		}
	}
	if n := los58bClusterCount(t, s, ""); n != 0 {
		t.Fatalf("%d rows written under the empty key, want 0", n)
	}
	if n := los58bClusterCount(t, s, "yesmem"); n != 0 {
		t.Fatalf("%d rows written under a bare name, want 0", n)
	}
}

// YM-424: reading clusters for an unassigned project must not return the rows that
// the old writer left under the empty key.
func TestLos58bClusterReadOfAnUnassignedProjectReturnsNothing(t *testing.T) {
	s := mustOpen(t)
	los58bClusterRow(t, s, "", "orphaned empty-key cluster", 1, 2)

	got, err := s.GetLearningClusters("")
	if err != nil {
		t.Fatalf("get clusters: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unassigned read returned %d rows from the empty key, want 0", len(got))
	}
}

// YM-423: a cluster set is derived data, and the sweep used to treat every key no live
// project maps to as disposable. The 68 legacy keys in the live database still describe
// 344 active learnings; deleting them throws away the only grouping of those rows,
// because no session ever triggers clustering for those projects again.
func TestLos58bPruneKeepsKeysThatStillDescribeLiveLearnings(t *testing.T) {
	s := mustOpenFiles(t)
	if err := s.UpsertSession(&models.Session{ID: "los58b-live", Project: "/home/testuser/projects/prune-live"}); err != nil {
		t.Fatalf("seed live session: %v", err)
	}

	live := los58bLearning(t, s, "/home/testuser/projects/prune-live", "los58b live learning")
	los58bClusterRow(t, s, "prune-legacy-referenced", "referenced", live)

	// A dead member: its only learning was superseded, so the grouping carries no
	// live meaning and is the sweep's business.
	old := los58bLearning(t, s, "/home/testuser/projects/prune-gone", "los58b superseded learning")
	replacement := los58bLearning(t, s, "/home/testuser/projects/prune-gone", "los58b replacement learning")
	if _, err := s.DB().Exec(`UPDATE learnings SET superseded_by = ? WHERE id = ?`, replacement, old); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	los58bClusterRow(t, s, "prune-legacy-superseded", "superseded", old)

	res, err := s.PruneOrphanLearningClusters(false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("dry run deleted %d rows", res.Deleted)
	}
	if !los58bContains(res.Kept, "prune-legacy-referenced") {
		t.Fatalf("kept = %v, want the key that still describes a live learning", res.Kept)
	}
	if !los58bContains(res.Orphans, "prune-legacy-superseded") {
		t.Fatalf("orphans = %v, want the key whose only member is superseded", res.Orphans)
	}
	if res.KeptRows != 1 {
		t.Fatalf("kept rows = %d, want 1", res.KeptRows)
	}

	res, err = s.PruneOrphanLearningClusters(true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n := los58bClusterCount(t, s, "prune-legacy-referenced"); n != 1 {
		t.Fatalf("%d rows left for the referenced key, want it kept", n)
	}
	if n := los58bClusterCount(t, s, "prune-legacy-superseded"); n != 0 {
		t.Fatalf("%d rows left for the superseded key, want it swept", n)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted %d rows, want 1", res.Deleted)
	}
}

// YM-423: the legacy keys are migrated, not deleted. A key is provable when every
// learning it references agrees on one absolute project path; everything else stays
// untouched and is reported.
func TestLos58bLegacyClusterKeyMigrationMovesOnlyProvableKeys(t *testing.T) {
	s := mustOpenFiles(t)

	const provable = "/home/testuser/projects/migrated"
	if err := s.UpsertSession(&models.Session{ID: "los58b-migrated", Project: provable}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	first := los58bLearning(t, s, provable, "los58b migrated member one")
	second := los58bLearning(t, s, provable+"/.worktrees/yesloop-los-58b", "los58b migrated member two")
	los58bClusterRow(t, s, "migrated", "old label", first, second)
	// Raw proxy state: SetClusterHash refuses a bare name (YM-424), but the hash of a key
	// written before that guard is exactly what the migration has to carry along.
	if err := s.SetProxyState("cluster_hash:migrated", "legacy-hash"); err != nil {
		t.Fatalf("seed hash: %v", err)
	}

	// No proof: one member carries no path.
	pathMember := los58bLearning(t, s, "/home/testuser/projects/noproof", "los58b path member")
	bareMember := los58bLearning(t, s, "noproof", "los58b bare member")
	los58bClusterRow(t, s, "noproof", "unprovable", pathMember, bareMember)

	// Conflict: two members name different repositories.
	left := los58bLearning(t, s, "/home/testuser/projects/left", "los58b left")
	right := los58bLearning(t, s, "/home/testuser/projects/right", "los58b right")
	los58bClusterRow(t, s, "split", "conflict", left, right)

	// Occupied: the target already carries its own, newer set.
	const occupied = "/home/testuser/projects/occupied"
	if err := s.UpsertSession(&models.Session{ID: "los58b-occupied", Project: occupied}); err != nil {
		t.Fatalf("seed occupied session: %v", err)
	}
	if err := s.ReplaceLearningClusters(occupied, []models.LearningCluster{los25bCluster("current")}); err != nil {
		t.Fatalf("seed current set: %v", err)
	}
	occupiedMember := los58bLearning(t, s, occupied, "los58b occupied member")
	los58bClusterRow(t, s, "occupied", "stale", occupiedMember)

	moves, skips, err := s.MigrateLegacyClusterKeys(false)
	if err != nil {
		t.Fatalf("dry-run migration: %v", err)
	}
	if len(moves) != 1 || moves[0].From != "migrated" || moves[0].To != provable || moves[0].Rows != 1 {
		t.Fatalf("dry-run moves = %+v, want exactly moved->%s with 1 row", moves, provable)
	}
	if n := los58bClusterCount(t, s, "migrated"); n != 1 {
		t.Fatalf("dry run moved rows already (%d under the old key)", n)
	}
	for _, want := range []string{"noproof", "split", "occupied"} {
		if !los58bSkipReason(skips, want) {
			t.Fatalf("skips = %+v, want %q reported as skipped", skips, want)
		}
	}

	moves, _, err = s.MigrateLegacyClusterKeys(true)
	if err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("apply moves = %+v, want 1", moves)
	}
	if n := los58bClusterCount(t, s, "migrated"); n != 0 {
		t.Fatalf("%d rows left under the legacy key after the migration", n)
	}
	if n := los58bClusterCount(t, s, provable); n != 1 {
		t.Fatalf("%d rows under the migrated key, want 1", n)
	}
	if got, _ := s.GetClusterHash(provable); got != "legacy-hash" {
		t.Fatalf("hash under the migrated key = %q, want it moved along", got)
	}
	if got, _ := s.GetProxyState("cluster_hash:migrated"); got != "" {
		t.Fatalf("legacy key kept its hash %q, so a key that comes back would believe it is current", got)
	}
	// The unprovable keys are untouched.
	if n := los58bClusterCount(t, s, "noproof"); n != 1 {
		t.Fatalf("%d rows under the unprovable key, want it untouched", n)
	}
	if n := los58bClusterCount(t, s, "split"); n != 1 {
		t.Fatalf("%d rows under the conflicting key, want it untouched", n)
	}
	if n := los58bClusterCount(t, s, "occupied"); n != 1 {
		t.Fatalf("%d rows under the occupied key, want it untouched", n)
	}
}

// YM-273 (1): the oldest cluster rows are keyed by a worktree PATH, which no reader
// reaches any more. The key space folds a worktree onto its repository, so the target
// follows from the key itself and needs no learning lookup. The live database still
// carries such keys.
func TestLos58bWorktreePathClusterKeyMigratesToItsRepository(t *testing.T) {
	s := mustOpenFiles(t)
	const repo = "/home/testuser/projects/wt-repo"
	const worktree = repo + "/.worktrees/yesloop-los-old"
	los58bClusterRow(t, s, worktree, "old label", 1, 2)

	moves, skips, err := s.MigrateLegacyClusterKeys(false)
	if err != nil {
		t.Fatalf("dry-run migration: %v", err)
	}
	if len(moves) != 1 || moves[0].From != worktree || moves[0].To != repo {
		t.Fatalf("dry-run moves = %+v, want %q -> %q", moves, worktree, repo)
	}
	if len(skips) != 0 {
		t.Fatalf("dry-run skips = %+v, want none", skips)
	}

	if _, _, err := s.MigrateLegacyClusterKeys(true); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if n := los58bClusterCount(t, s, worktree); n != 0 {
		t.Fatalf("%d rows left under the worktree key", n)
	}
	if n := los58bClusterCount(t, s, repo); n != 1 {
		t.Fatalf("%d rows under the repository key, want 1", n)
	}

	// Once the repository key carries its own set, a second worktree key stays put:
	// merging would mix two groupings of the same repository.
	los58bClusterRow(t, s, worktree+"-second", "second", 3)
	moves, skips, err = s.MigrateLegacyClusterKeys(true)
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if len(moves) != 0 {
		t.Fatalf("moves = %+v, want none while the repository key is taken", moves)
	}
	if !los58bSkipReason(skips, worktree+"-second") {
		t.Fatalf("skips = %+v, want the worktree key reported as skipped", skips)
	}
	if n := los58bClusterCount(t, s, worktree+"-second"); n != 1 {
		t.Fatalf("%d rows under the worktree key, want it untouched", n)
	}
}

// YM-424: the bare-name key space is closed for new writes, not for the data already in
// it. A caller that names a legacy key gets exactly those rows; an unassigned read stays
// empty, so the two states remain distinguishable.
func TestLos58bLegacyKeyStaysReadableByItsOwnName(t *testing.T) {
	s := mustOpen(t)
	los58bClusterRow(t, s, "dokumente-legacy", "old label", 1, 2)

	got, err := s.GetLearningClusters("dokumente-legacy")
	if err != nil {
		t.Fatalf("get legacy clusters: %v", err)
	}
	if len(got) != 1 || got[0].Label != "old label" {
		t.Fatalf("legacy read = %+v, want the row stored under that name", got)
	}
	unassigned, err := s.GetLearningClusters("")
	if err != nil {
		t.Fatalf("get unassigned clusters: %v", err)
	}
	if len(unassigned) != 0 {
		t.Fatalf("unassigned read returned %d rows, want 0", len(unassigned))
	}
}

// YM-424: the rename has to refuse a target without a path instead of writing its cluster
// rows under the empty key, where no reader looks.
func TestLos58bMigrateProjectRefusesATargetWithoutAPath(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.MigrateProject("/home/testuser/projects/a", "b", "a", "b"); err == nil {
		t.Fatal("MigrateProject accepted a relative target, so its cluster rows would land under the empty key")
	}
	if _, err := s.MigrateProjectDryRun("/home/testuser/projects/a", "b", "a", "b"); err == nil {
		t.Fatal("the dry run did not report the same refusal as the write path")
	}
}

// YM-423: an unreadable membership is not an empty one. A cluster row whose learning_ids
// cannot be parsed must keep its key, otherwise the sweep would delete the only grouping
// of learnings it never managed to read.
func TestLos58bPruneKeepsAKeyWithUnreadableMembership(t *testing.T) {
	s := mustOpenFiles(t)
	if err := s.UpsertSession(&models.Session{ID: "los58b-unreadable-live", Project: "/home/testuser/projects/prune-live"}); err != nil {
		t.Fatalf("seed live session: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO learning_clusters (project, label, learning_count, learning_ids)
		VALUES ('prune-legacy-unreadable', 'broken membership', 2, 'not json')`); err != nil {
		t.Fatalf("insert unreadable row: %v", err)
	}

	res, err := s.PruneOrphanLearningClusters(true)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !los58bContains(res.Kept, "prune-legacy-unreadable") {
		t.Fatalf("kept = %v, want the key with the unreadable membership", res.Kept)
	}
	if n := los58bClusterCount(t, s, "prune-legacy-unreadable"); n != 1 {
		t.Fatalf("%d rows left, want the row kept", n)
	}
}

// YM-423: two keys can resolve to the same repository. The first one moves, the second is
// reported instead of being merged into the target the first one just took.
func TestLos58bMigrationLetsOnlyOneKeyTakeAFreeTarget(t *testing.T) {
	s := mustOpenFiles(t)
	const target = "/home/testuser/projects/shared"
	member := los58bLearning(t, s, target, "los58b shared member")
	los58bClusterRow(t, s, "shared-old-a", "a", member)
	los58bClusterRow(t, s, "shared-old-b", "b", member)

	moves, skips, err := s.MigrateLegacyClusterKeys(true)
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	if len(moves) != 1 || moves[0].To != target {
		t.Fatalf("moves = %+v, want exactly one move to %q", moves, target)
	}
	if !los58bSkipReason(skips, "shared-old-b") {
		t.Fatalf("skips = %+v, want the second key reported", skips)
	}
	if n := los58bClusterCount(t, s, target); n != 1 {
		t.Fatalf("%d rows under the target key, want 1 (no merge)", n)
	}
}

// YM-424 review (security): a relative source path has no cluster key. It must not
// degenerate to the empty key and drag the unassigned rows onto the rename target.
func TestLos58bMigrateProjectLeavesUnassignedRowsAlone(t *testing.T) {
	s := mustOpen(t)
	los58bClusterRow(t, s, "", "unassigned rows", 1, 2)

	if _, err := s.MigrateProject("src/foo", "/home/testuser/target", "foo", "target"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n := los58bClusterCount(t, s, ""); n != 1 {
		t.Fatalf("%d rows left under the empty key, want the unassigned row untouched", n)
	}
	if n := los58bClusterCount(t, s, "/home/testuser/target"); n != 0 {
		t.Fatalf("%d rows moved to the rename target, want 0", n)
	}
}

func los58bContains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func los58bSkipReason(skips []ClusterKeySkip, key string) bool {
	for _, s := range skips {
		if s.Key == key && s.Reason != "" {
			return true
		}
	}
	return false
}

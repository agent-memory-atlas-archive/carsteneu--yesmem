package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/carsteneu/yesmem/internal/models"
)

// canonicalClusterKey normalises a project path or short name to the key under
// which cluster rows live. One repository is one key: every worktree of it shares
// the key of its root, and two repositories that share a basename keep separate
// keys (models.ClusterProjectKey).
func canonicalClusterKey(project string) string {
	return models.ClusterProjectKey(project)
}

// ReplaceLearningClusters atomically replaces all clusters for a project.
// Deletes existing clusters in a transaction, then inserts the new ones.
// A project without a repository key (a bare name, an empty value) is refused: the
// writer must not mint a key space of its own, it would only produce rows no reader
// can reach (YM-424).
func (s *Store) ReplaceLearningClusters(project string, clusters []models.LearningCluster) error {
	project = canonicalClusterKey(project)
	if project == "" {
		return fmt.Errorf("replace learning clusters: project names no repository")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM learning_clusters WHERE project = ?`, project); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete old clusters: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO learning_clusters
		(project, label, learning_count, avg_recency_days, avg_hit_count, confidence, learning_ids)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	for _, c := range clusters {
		if _, err := stmt.Exec(project, c.Label, c.LearningCount, c.AvgRecencyDays, c.AvgHitCount, c.Confidence, c.LearningIDs); err != nil {
			tx.Rollback()
			return fmt.Errorf("insert cluster %q: %w", c.Label, err)
		}
	}

	return tx.Commit()
}

// GetLearningClusters returns all clusters for a project, ordered by confidence DESC.
// The project is read through the cluster key, so callers may pass a worktree path or
// the repository path. An empty value names no project and has no set (YM-424).
func (s *Store) GetLearningClusters(project string) ([]models.LearningCluster, error) {
	key := canonicalClusterKey(project)
	if key != "" {
		return s.learningClusters(key)
	}
	if models.ClassifyProject(project) != models.ProjectOriginLegacy {
		return nil, nil
	}
	// A legacy bare name is read from exactly the rows stored under that name: it is what
	// the caller asked for, and those rows are the only handle a project without a path
	// has. Only when nothing is stored under it does the name resolve through the rows of
	// the one repository that carries it; when two repositories share the name, returning
	// either set would mix their clusters.
	stored, err := s.learningClusters(project)
	if err != nil || len(stored) > 0 {
		return stored, err
	}
	resolved, err := s.uniqueClusterKeyByBasename(project)
	if err != nil || resolved == "" {
		return nil, err
	}
	return s.learningClusters(resolved)
}

// learningClusters reads the rows stored under exactly one key.
func (s *Store) learningClusters(key string) ([]models.LearningCluster, error) {
	rows, err := s.readerDB().Query(`SELECT id, project, label, learning_count, avg_recency_days,
		avg_hit_count, confidence, learning_ids
		FROM learning_clusters WHERE project = ?
		ORDER BY confidence DESC`, key)
	if err != nil {
		return nil, fmt.Errorf("get learning clusters: %w", err)
	}
	defer rows.Close()

	var clusters []models.LearningCluster
	for rows.Next() {
		var c models.LearningCluster
		if err := rows.Scan(&c.ID, &c.Project, &c.Label, &c.LearningCount,
			&c.AvgRecencyDays, &c.AvgHitCount, &c.Confidence, &c.LearningIDs); err != nil {
			return nil, fmt.Errorf("scan cluster: %w", err)
		}
		clusters = append(clusters, c)
	}
	return clusters, rows.Err()
}

// uniqueClusterKeyByBasename returns the one stored key that ends in "/<name>",
// or "" when no key or more than one key matches.
func (s *Store) uniqueClusterKeyByBasename(name string) (string, error) {
	suffix := "/" + name
	rows, err := s.readerDB().Query(`SELECT DISTINCT project FROM learning_clusters
		WHERE substr(project, -length(?)) = ?`, suffix, suffix)
	if err != nil {
		return "", fmt.Errorf("resolve cluster key %q: %w", name, err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return "", err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(keys) != 1 {
		return "", nil
	}
	return keys[0], nil
}

// GetClusterHash returns the stored change-detection hash for a project's clustering.
// A project without a repository key has none (YM-424): a bare name must not read the
// hash of whichever repository it might belong to.
func (s *Store) GetClusterHash(project string) (string, error) {
	key := canonicalClusterKey(project)
	if key == "" {
		return "", nil
	}
	return s.GetProxyState("cluster_hash:" + key)
}

// SetClusterHash stores the change-detection hash for a project's clustering.
func (s *Store) SetClusterHash(project, hash string) error {
	key := canonicalClusterKey(project)
	if key == "" {
		return nil
	}
	return s.SetProxyState("cluster_hash:"+key, hash)
}

// clusterCategoryExclusion keeps the fingerprint aligned with the set clustering
// actually reads: these categories are dropped before clustering.
const clusterCategoryExclusion = `category NOT IN ('narrative','unfinished','strategic','recurrence_alert')`

// ClusterChangeFingerprint hashes the learning set that clustering reads for a
// learnings scope. The scope is models.CanonicalProject, NOT the cluster row key:
// the learning rows carry the repository basename, the cluster rows carry the
// repository root, and the fingerprint has to match the rows clustering reads.
// It must count canonical_project rather than project: learnings written inside a
// worktree carry the worktree path as project, so a per-path fingerprint never
// sees them. Global learnings (canonical_project = ”) belong to no repository and
// clustering skips them, so they are not counted here either; otherwise one global
// learning re-clustered every project.
//
// Since YM-321 the membership is narrowed per repository root on top of this scope,
// while the fingerprint stays on the whole basename scope. That is deliberate: the
// fingerprint has to over-approximate what clustering reads, otherwise a change in
// a counted-out row could go unnoticed. Counting a same-named repository's
// learnings only costs a re-clustering run whose result is unchanged.
//
// Alignment with the clustering input is by category and scope only: expiry and a
// later embedding backfill do not change the hash, so a cluster can keep a member
// that has since expired until some other change re-triggers clustering.
func (s *Store) ClusterChangeFingerprint(scope string) (string, error) {
	if scope == "" {
		return "", nil
	}
	scope = models.CanonicalProject(scope)

	var active int
	var maxCreated sql.NullString
	if err := s.readerDB().QueryRow(`SELECT COUNT(*), MAX(created_at) FROM learnings
		WHERE superseded_by IS NULL AND canonical_project = ?
		AND `+clusterCategoryExclusion, scope).Scan(&active, &maxCreated); err != nil {
		return "", fmt.Errorf("cluster fingerprint: %w", err)
	}

	// Distillation and evolution supersede rows without creating new ones, so the
	// active count alone would keep a distilled cluster unchanged forever. A row
	// whose successor was hard-deleted is counted apart: RepairDanglingSupersedeEdges
	// turns a positive edge into that sentinel, and with one shared counter the hash
	// would not notice the edge losing its target (YM-443).
	var superseded, detached int
	if err := s.readerDB().QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN superseded_by = ? THEN 0 ELSE 1 END), 0),
		COALESCE(SUM(CASE WHEN superseded_by = ? THEN 1 ELSE 0 END), 0)
		FROM learnings
		WHERE canonical_project = ? AND superseded_by IS NOT NULL
		AND `+clusterCategoryExclusion, SupersededByDeleted, SupersededByDeleted, scope).Scan(&superseded, &detached); err != nil {
		return "", fmt.Errorf("cluster fingerprint superseded: %w", err)
	}

	raw := fmt.Sprintf("a:%d:%s|x:%d|d:%d", active, maxCreated.String, superseded, detached)
	h := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", h[:16]), nil
}

// pruneClustersPreCommitHook is a test seam: it runs inside the row transaction, after
// the deletes and before the commit, so a test can prove that the change-detection
// hashes are already gone at that moment (YM-390).
var pruneClustersPreCommitHook atomic.Pointer[func()]

// ClusterPruneResult is what one cluster sweep saw and did. Orphans are the keys with
// no live project and no live learning behind them, the ones a sweep may delete. Kept
// are the unreachable keys whose rows still name a live learning: the sweep leaves
// them alone (YM-423), because for a project without sessions no later clustering run
// would rebuild that grouping.
type ClusterPruneResult struct {
	Orphans  []string
	Kept     []string
	Deleted  int64
	KeptRows int64
}

// PruneOrphanLearningClusters reports the cluster keys that no live project maps
// to and deletes those rows when apply is set. Keys become orphans when the key
// space moves (cluster keys were basenames and per-worktree paths before YM-273)
// or when a project loses its last session row: ReplaceLearningClusters only ever
// deletes the key it writes, so nothing else would ever collect them.
//
// A repository is live while sessions name it (ListProjects). An empty live set
// therefore means "no project known", not "every cluster orphaned", and the sweep
// returns without touching anything.
//
// Being unreachable is not enough: a key whose rows still name a live learning is
// kept and reported, because that grouping is the only record of those rows. The
// live database carries 68 such legacy keys over 344 active learnings, and no
// session would ever re-cluster them.
func (s *Store) PruneOrphanLearningClusters(apply bool) (*ClusterPruneResult, error) {
	withRows, err := s.clusterKeysWithRows()
	if err != nil {
		return nil, fmt.Errorf("prune orphan clusters: %w", err)
	}
	live, err := s.liveClusterKeys()
	if err != nil {
		return nil, fmt.Errorf("prune orphan clusters: %w", err)
	}
	if len(live) == 0 {
		return &ClusterPruneResult{}, nil
	}
	referenced, err := s.clusterKeysWithLiveLearnings()
	if err != nil {
		return nil, fmt.Errorf("prune orphan clusters: %w", err)
	}

	res := &ClusterPruneResult{}
	for key, rows := range withRows {
		if live[key] {
			continue
		}
		if _, ok := referenced[key]; ok {
			res.Kept = append(res.Kept, key)
			res.KeptRows += rows
			continue
		}
		res.Orphans = append(res.Orphans, key)
	}
	sort.Strings(res.Orphans)
	sort.Strings(res.Kept)
	if !apply || len(res.Orphans) == 0 {
		return res, nil
	}

	// The change-detection hash goes BEFORE the rows commit. A crash between the two
	// leaves rows without a hash, which the next clustering run rebuilds. The other
	// order left a surviving hash for a key whose rows were already gone, and a key that
	// comes back then believes its clusters are current, so they would never be rebuilt
	// (YM-390).
	for _, key := range res.Orphans {
		if err := s.DeleteProxyState("cluster_hash:" + key); err != nil {
			return res, fmt.Errorf("prune cluster hash %q: %w", key, err)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return res, fmt.Errorf("prune orphan clusters: begin tx: %w", err)
	}
	var deleted int64
	for _, key := range res.Orphans {
		res2, err := tx.Exec(`DELETE FROM learning_clusters WHERE project = ?`, key)
		if err != nil {
			tx.Rollback()
			res.Deleted = deleted
			return res, fmt.Errorf("prune orphan clusters %q: %w", key, err)
		}
		n, err := res2.RowsAffected()
		if err != nil {
			tx.Rollback()
			res.Deleted = deleted
			return res, fmt.Errorf("prune orphan clusters %q: %w", key, err)
		}
		deleted += n
	}
	if hook := pruneClustersPreCommitHook.Load(); hook != nil {
		(*hook)()
	}
	if err := tx.Commit(); err != nil {
		res.Deleted = deleted
		return res, fmt.Errorf("prune orphan clusters: commit: %w", err)
	}
	res.Deleted = deleted
	return res, nil
}

// liveClusterKeys is the set of cluster keys a known project maps to. An empty set
// means "no project known", never "every key is orphaned".
func (s *Store) liveClusterKeys() (map[string]bool, error) {
	projects, err := s.ListProjects()
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(projects))
	for _, g := range GroupByCanonicalProject(projects) {
		live[g.ClusterKey] = true
	}
	return live, nil
}

// clusterKeysWithRows returns the keys that still carry cluster rows, with the number
// of rows per key.
func (s *Store) clusterKeysWithRows() (map[string]int64, error) {
	rows, err := s.readerDB().Query(`SELECT project FROM learning_clusters`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make(map[string]int64)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys[key]++
	}
	return keys, rows.Err()
}

// clusterLearningBatch bounds the learning IDs one statement may bind: SQLite rejects a
// statement above 32766 variables.
const clusterLearningBatch = 500

// clusterKeysWithLiveLearnings returns the keys whose cluster rows still name at least
// one live learning. The grouping is derived data, but for a project without sessions no
// later clustering run rebuilds it, so the sweep must leave those keys alone (YM-423). A
// row whose learning_ids cannot be read counts as a live reference: an unreadable
// membership is not evidence of an empty one.
func (s *Store) clusterKeysWithLiveLearnings() (map[string]bool, error) {
	rows, err := s.readerDB().Query(`SELECT project, learning_ids FROM learning_clusters`)
	if err != nil {
		return nil, err
	}

	owners := make(map[int64][]string)
	referenced := make(map[string]bool)
	for rows.Next() {
		var key string
		var raw sql.NullString
		if err := rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		ids, ok := parseClusterLearningIDs(raw.String)
		if !ok {
			referenced[key] = true
			continue
		}
		for _, id := range ids {
			owners[id] = appendUniqueKey(owners[id], key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// The batch queries below need the connection pool; the cursor is done anyway.
	rows.Close()

	ids := make([]int64, 0, len(owners))
	for id := range owners {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for start := 0; start < len(ids); start += clusterLearningBatch {
		end := start + clusterLearningBatch
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		placeholders := placeholdersFor(len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		found, err := s.readerDB().Query(
			`SELECT id FROM learnings WHERE superseded_by IS NULL AND id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for found.Next() {
			var id int64
			if err := found.Scan(&id); err != nil {
				found.Close()
				return nil, err
			}
			for _, key := range owners[id] {
				referenced[key] = true
			}
		}
		if err := found.Err(); err != nil {
			found.Close()
			return nil, err
		}
		found.Close()
	}
	return referenced, nil
}

// parseClusterLearningIDs reads the JSON list stored in learning_clusters.learning_ids.
// ok is false when the value is unreadable, which the caller treats as a live reference.
func parseClusterLearningIDs(raw string) ([]int64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, true
	}
	var ids []int64
	if err := json.Unmarshal([]byte(trimmed), &ids); err != nil {
		return nil, false
	}
	return ids, true
}

// appendUniqueKey appends key unless the list already carries it.
func appendUniqueKey(list []string, key string) []string {
	for _, have := range list {
		if have == key {
			return list
		}
	}
	return append(list, key)
}

// placeholdersFor returns the "?,?,?" list for n bind variables.
func placeholdersFor(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// PruneOrphanClusterHashes reports the change-detection hashes whose cluster key
// neither a live project maps to nor still carries cluster rows, and deletes them
// when apply is set. The key space moved from basenames and worktree paths to
// repository roots (YM-273) and the hashes of the old keys stayed behind: the row
// sweep only ever removed hashes of keys that still had rows. A surviving hash is
// not harmless, it makes a key that comes back believe its clusters are current, so
// they would never be rebuilt.
//
// The live set is read from yesmem.db and the keys from the store's proxy state
// (runtime.db), unlike the row sweep which stays in one file. A read-only handle
// cannot see runtime.db at all: it opens runtimeDB = yesmem.db, so this sweep is
// for the write path.
func (s *Store) PruneOrphanClusterHashes(apply bool) ([]string, int64, error) {
	withRows, err := s.clusterKeysWithRows()
	if err != nil {
		return nil, 0, fmt.Errorf("prune orphan cluster hashes: %w", err)
	}
	live, err := s.liveClusterKeys()
	if err != nil {
		return nil, 0, fmt.Errorf("prune orphan cluster hashes: %w", err)
	}
	if len(live) == 0 {
		return nil, 0, nil
	}
	keys, err := s.proxyStateKeysWithPrefix("cluster_hash:")
	if err != nil {
		return nil, 0, fmt.Errorf("prune orphan cluster hashes: %w", err)
	}
	// A key that still carries rows belongs to the row sweep, which removes the rows
	// and the hash together. Sweeping its hash here would take the change detection
	// away from rows that are still being served.
	keep := make(map[string]bool, len(live)+len(withRows))
	for key := range live {
		keep["cluster_hash:"+key] = true
	}
	for key := range withRows {
		keep["cluster_hash:"+key] = true
	}

	var dead []string
	for _, key := range keys {
		if !keep[key] {
			dead = append(dead, key)
		}
	}
	sort.Strings(dead)
	if !apply || len(dead) == 0 {
		return dead, 0, nil
	}
	deleted, err := s.PruneProxyStatePrefix("cluster_hash:", keep)
	if err != nil {
		return dead, deleted, fmt.Errorf("prune orphan cluster hashes: %w", err)
	}
	return dead, deleted, nil
}

// proxyStateKeysWithPrefix lists the proxy_state keys under prefix. substr instead
// of LIKE: a project path may contain wildcard characters.
func (s *Store) proxyStateKeysWithPrefix(prefix string) ([]string, error) {
	rows, err := s.proxyStateDB().Query(
		`SELECT key FROM proxy_state WHERE substr(key, 1, ?) = ?`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

package storage

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/carsteneu/yesmem/internal/models"
)

// ClusterKeyMove is one stale cluster key the migration re-points.
type ClusterKeyMove struct {
	From string
	To   string
	Rows int64
}

// ClusterKeySkip is one stale cluster key that stays where it is, with the reason.
type ClusterKeySkip struct {
	Key    string
	Reason string
}

// MigrateLegacyClusterKeys re-points the cluster keys the old key spaces left behind to
// the key the current rules give, and reports every key it leaves alone (YM-423).
//
// Two kinds of key are stale, and they need different proof:
//
//   - A key that is a PATH inside a worktree checkout. The key space folds worktrees
//     onto their repository (models.ClusterProjectKey), so the target follows from the
//     key itself.
//   - A key that is a bare basename. The target follows from the learnings the key's
//     rows reference, and only when they all carry an absolute project path that
//     resolves to one key. A basename is never mapped to a path because some known
//     repository happens to carry that name: the live database has a key "Dokumente"
//     whose 6 learnings carry no path at all, while a repository of that name exists.
//
// A target that already carries its own cluster set is not written to: the two sets
// describe the same repository in different states, and the target is the newer one.
// Such keys are reported and left alone, and the sweep keeps them for as long as their
// learnings live.
func (s *Store) MigrateLegacyClusterKeys(apply bool) ([]ClusterKeyMove, []ClusterKeySkip, error) {
	rows, err := s.readerDB().Query(`SELECT project, learning_ids FROM learning_clusters`)
	if err != nil {
		return nil, nil, fmt.Errorf("migrate legacy cluster keys: %w", err)
	}
	rowCount := make(map[string]int64)
	members := make(map[string][]int64)
	unreadable := make(map[string]bool)
	var keys []string
	for rows.Next() {
		var key string
		var raw sql.NullString
		if err := rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("migrate legacy cluster keys: %w", err)
		}
		if rowCount[key] == 0 {
			keys = append(keys, key)
		}
		rowCount[key]++
		ids, ok := parseClusterLearningIDs(raw.String)
		if !ok {
			unreadable[key] = true
			continue
		}
		members[key] = append(members[key], ids...)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("migrate legacy cluster keys: %w", err)
	}
	rows.Close()
	sort.Strings(keys)

	var moves []ClusterKeyMove
	var skips []ClusterKeySkip
	planned := make(map[string]string)
	for _, key := range keys {
		if key == "" {
			skips = append(skips, ClusterKeySkip{Key: key, Reason: "the empty key names no project"})
			continue
		}
		if models.ClusterProjectKey(key) == key {
			continue // already in the current key space
		}
		target, reason, err := s.legacyClusterKeyTarget(key, members[key], unreadable[key])
		if err != nil {
			return moves, skips, err
		}
		if target == "" {
			skips = append(skips, ClusterKeySkip{Key: key, Reason: reason})
			continue
		}
		// The listing has to say what --apply would do, so the occupancy check belongs
		// here and not only in the write path.
		if rowCount[target] > 0 {
			skips = append(skips, ClusterKeySkip{
				Key:    key,
				Reason: fmt.Sprintf("target key %q already carries its own cluster set", target),
			})
			continue
		}
		if other, ok := planned[target]; ok {
			skips = append(skips, ClusterKeySkip{
				Key:    key,
				Reason: fmt.Sprintf("target key %q is taken by %q in this run", target, other),
			})
			continue
		}
		planned[target] = key
		moves = append(moves, ClusterKeyMove{From: key, To: target, Rows: rowCount[key]})
	}

	if !apply {
		return moves, skips, nil
	}
	applied := make([]ClusterKeyMove, 0, len(moves))
	taken := make(map[string]bool)
	for _, mv := range moves {
		if taken[mv.To] {
			skips = append(skips, ClusterKeySkip{
				Key:    mv.From,
				Reason: fmt.Sprintf("target key %q is taken by a key migrated in this run", mv.To),
			})
			continue
		}
		moved, err := s.moveClusterKey(mv)
		if err != nil {
			return applied, skips, err
		}
		if !moved {
			skips = append(skips, ClusterKeySkip{
				Key:    mv.From,
				Reason: fmt.Sprintf("target key %q already carries its own cluster set", mv.To),
			})
			continue
		}
		taken[mv.To] = true
		applied = append(applied, mv)
	}
	sort.Slice(skips, func(i, j int) bool { return skips[i].Key < skips[j].Key })
	return applied, skips, nil
}

// legacyClusterKeyTarget returns the key one stale key has to move to, or "" plus the
// reason why it has to stay.
func (s *Store) legacyClusterKeyTarget(key string, ids []int64, unreadable bool) (string, string, error) {
	// A path key inside a worktree: the target follows from the key itself.
	if models.ClassifyProject(key) == models.ProjectOriginPath {
		if target := models.ClusterProjectKey(key); target != "" && target != key {
			return target, "", nil
		}
		return "", "key already names its repository", nil
	}

	if unreadable {
		return "", "learning_ids cannot be read, so the membership is unknown", nil
	}
	if len(ids) == 0 {
		return "", "no learning references", nil
	}
	projects, err := s.learningProjectsByID(ids)
	if err != nil {
		return "", "", err
	}
	if len(projects) == 0 {
		return "", "no referenced learning exists any more", nil
	}

	target := ""
	var firstProject string
	order := make([]int64, 0, len(projects))
	for id := range projects {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, id := range order {
		project := projects[id]
		if models.ClassifyProject(project) != models.ProjectOriginPath {
			return "", fmt.Sprintf("learning %d names no path (project %q)", id, project), nil
		}
		candidate := models.ClusterProjectKey(project)
		if candidate == "" {
			return "", fmt.Sprintf("learning %d names no repository (project %q)", id, project), nil
		}
		if target == "" {
			target, firstProject = candidate, project
			continue
		}
		if candidate != target {
			return "", fmt.Sprintf("learnings name different repositories (%q via %q and %q via %q)",
				target, firstProject, candidate, project), nil
		}
	}
	if target == "" {
		return "", "no repository follows from the referenced learnings", nil
	}
	return target, "", nil
}

// learningProjectsByID returns the project of every learning row that still exists.
// The IDs are read from the cluster rows, so they are spread over several batches: a
// single materialized list would exceed SQLite's variable bound.
func (s *Store) learningProjectsByID(ids []int64) (map[int64]string, error) {
	unique := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })

	out := make(map[int64]string, len(unique))
	for start := 0; start < len(unique); start += clusterLearningBatch {
		end := start + clusterLearningBatch
		if end > len(unique) {
			end = len(unique)
		}
		batch := unique[start:end]
		placeholders := placeholdersFor(len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.readerDB().Query(
			`SELECT id, COALESCE(project, '') FROM learnings WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("read learning projects: %w", err)
		}
		for rows.Next() {
			var id int64
			var project string
			if err := rows.Scan(&id, &project); err != nil {
				rows.Close()
				return nil, fmt.Errorf("read learning projects: %w", err)
			}
			out[id] = project
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read learning projects: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// moveClusterKey re-points the rows and the change-detection hash of one key. It reports
// false when the target gained rows in the meantime, in which case nothing is written.
func (s *Store) moveClusterKey(mv ClusterKeyMove) (bool, error) {
	// The hash goes first: a crash after it costs one rebuild, while a surviving hash
	// under the old key would make a returning key believe its clusters are current. The
	// same rebuild covers the occupancy case below, where the rows stay and only the hash
	// is gone.
	hash, err := s.GetProxyState("cluster_hash:" + mv.From)
	if err != nil {
		return false, fmt.Errorf("read cluster hash %q: %w", mv.From, err)
	}
	if err := s.DeleteProxyState("cluster_hash:" + mv.From); err != nil {
		return false, fmt.Errorf("drop cluster hash %q: %w", mv.From, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("migrate cluster key %q: begin tx: %w", mv.From, err)
	}
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM learning_clusters WHERE project = ?`, mv.To).Scan(&existing); err != nil {
		tx.Rollback()
		return false, fmt.Errorf("migrate cluster key %q: %w", mv.From, err)
	}
	if existing > 0 {
		tx.Rollback()
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE learning_clusters SET project = ? WHERE project = ?`, mv.To, mv.From); err != nil {
		tx.Rollback()
		return false, fmt.Errorf("migrate cluster key %q: %w", mv.From, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("migrate cluster key %q: commit: %w", mv.From, err)
	}
	if hash != "" {
		if err := s.SetProxyState("cluster_hash:"+mv.To, hash); err != nil {
			return false, fmt.Errorf("carry cluster hash to %q: %w", mv.To, err)
		}
	}
	return true, nil
}

package storage

import (
	"os"
	"strings"
	"time"
)

// PruneOrphanedScanState deletes scan bookkeeping that can never be used again:
// project_scan rows whose key names a directory that no longer exists (or, for a
// key from the retired basename scheme, whose project has no session at all or
// whose recorded session directories are all gone), and scan_cooldown rows older
// than maxAge.
//
// An absolute key is judged by its directory: a repository that still exists
// keeps its row even when no session names it, because a missing session is not
// evidence of a dead project. A project whose path cannot be determined is kept
// as well. One exception disarms both rules: when the session table yields no
// resolvable path at all, an empty live set says nothing about any project, so
// no scan row is deleted. Expired cooldown rows go regardless: they carry their
// own age. Deleting a scan row only costs a re-scan, so a wrong deletion is
// cheap; deleting a live row would be pure churn.
//
// projectKey resolves a directory to the scan cache key (codescan.ScanCacheKey).
// The cache is keyed by the enclosing checkout root, sessions by absolute path,
// so both sides are mapped through the same function. The resolver walks the
// session directory, so callers accept the same file access the scanner already
// performs.
func (s *Store) PruneOrphanedScanState(maxAge time.Duration, projectKey func(dir string) string) (scans, cooldowns int64, err error) {
	live, err := s.sessionPathsByKey(projectKey)
	if err != nil {
		return 0, 0, err
	}

	rows, err := s.db.Query(`SELECT project FROM project_scan`)
	if err != nil {
		return 0, 0, err
	}
	var orphans []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if scanKeyIsOrphan(key, live) {
			orphans = append(orphans, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	// An empty live set means the session table carries no resolvable path (for
	// example an installation whose rows predate absolute paths). Pruning then
	// would empty the cache for every project at once, so skip it.
	if len(live) == 0 {
		orphans = nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	for _, key := range orphans {
		if _, err := tx.Exec(`DELETE FROM project_scan_files WHERE project = ?`, key); err != nil {
			return 0, 0, err
		}
		res, err := tx.Exec(`DELETE FROM project_scan WHERE project = ?`, key)
		if err != nil {
			return 0, 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			scans += n
		}
	}

	res, err := tx.Exec(`DELETE FROM scan_cooldown WHERE failed_at < ?`, time.Now().Add(-maxAge).Unix())
	if err != nil {
		return 0, 0, err
	}
	cooldowns, _ = res.RowsAffected()
	return scans, cooldowns, tx.Commit()
}

// sessionPathsByKey groups the absolute project directories of all sessions by
// their scan cache key.
func (s *Store) sessionPathsByKey(projectKey func(dir string) string) (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT project FROM sessions WHERE project LIKE '/%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		key := projectKey(p)
		out[key] = append(out[key], p)
	}
	return out, rows.Err()
}

// scanKeyIsOrphan reports whether no live session directory backs the scan key.
// Keys are checkout roots (codescan.ScanCacheKey), so they are checked
// directly; a key without a leading separator stems from the retired
// basename scheme and is judged against the session map.
func scanKeyIsOrphan(key string, live map[string][]string) bool {
	if strings.HasPrefix(key, "/") {
		_, statErr := os.Stat(key)
		return os.IsNotExist(statErr)
	}
	paths, ok := live[key]
	if !ok {
		return true
	}
	for _, p := range paths {
		if _, statErr := os.Stat(p); statErr == nil || !os.IsNotExist(statErr) {
			return false
		}
	}
	return true
}

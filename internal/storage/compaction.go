package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CompactedBlock represents a group of stage-3 stubs compressed into a summary.
type CompactedBlock struct {
	ID        int64  `json:"id"`
	ThreadID  string `json:"thread_id"`
	StartIdx  int    `json:"start_idx"`
	EndIdx    int    `json:"end_idx"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at,omitempty"`
}

// SaveCompactedBlock stores a new compacted block (upsert by thread_id + start_idx).
// No production path calls it: the collapse path keeps its archive in the frozen
// prefix, and get_compacted_stubs plus expand_context read the legacy table as a
// fallback. Written only by tests that seed those fallbacks.
func (s *Store) SaveCompactedBlock(threadID string, startIdx, endIdx int, content string) error {
	_, err := s.db.Exec(`
		INSERT INTO compacted_blocks (thread_id, start_idx, end_idx, content)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(thread_id, start_idx) DO UPDATE SET
			end_idx = excluded.end_idx,
			content = excluded.content`,
		threadID, startIdx, endIdx, content,
	)
	if err != nil {
		return fmt.Errorf("save compacted block: %w", err)
	}
	return nil
}

// GetCompactedBlocks returns all compacted blocks for a thread, ordered by start_idx.
func (s *Store) GetCompactedBlocks(threadID string) ([]*CompactedBlock, error) {
	rows, err := s.readerDB().Query(`
		SELECT id, thread_id, start_idx, end_idx, content, created_at
		FROM compacted_blocks
		WHERE thread_id = ?
		ORDER BY start_idx ASC`, threadID)
	if err != nil {
		return nil, fmt.Errorf("get compacted blocks: %w", err)
	}
	defer rows.Close()

	return scanCompactedBlocks(rows)
}

// GetCompactedBlocksInRange returns blocks that overlap with the given message index range.
func (s *Store) GetCompactedBlocksInRange(threadID string, fromIdx, toIdx int) ([]*CompactedBlock, error) {
	rows, err := s.readerDB().Query(`
		SELECT id, thread_id, start_idx, end_idx, content, created_at
		FROM compacted_blocks
		WHERE thread_id = ? AND start_idx < ? AND end_idx > ?
		ORDER BY start_idx ASC`, threadID, toIdx, fromIdx)
	if err != nil {
		return nil, fmt.Errorf("get compacted blocks in range: %w", err)
	}
	defer rows.Close()

	return scanCompactedBlocks(rows)
}

// DeleteCompactedBlocks removes all compacted blocks for a thread.
func (s *Store) DeleteCompactedBlocks(threadID string) error {
	_, err := s.db.Exec(`DELETE FROM compacted_blocks WHERE thread_id = ?`, threadID)
	return err
}

func scanCompactedBlocks(rows interface {
	Next() bool
	Scan(...any) error
}) ([]*CompactedBlock, error) {
	var blocks []*CompactedBlock
	for rows.Next() {
		b := &CompactedBlock{}
		if err := rows.Scan(&b.ID, &b.ThreadID, &b.StartIdx, &b.EndIdx, &b.Content, &b.CreatedAt); err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

// SetProxyState stores a key-value pair in proxy_state (upsert).
func (s *Store) SetProxyState(key, value string) error {
	_, err := s.proxyStateDB().Exec(`INSERT INTO proxy_state (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=CURRENT_TIMESTAMP`, key, value)
	return err
}

// PruneProxyStatePrefix deletes every key under prefix that is not in keep.
// Prefix matching is done with substr, not LIKE: project names may contain
// wildcard characters. Returns the number of deleted rows.
func (s *Store) PruneProxyStatePrefix(prefix string, keep map[string]bool) (int64, error) {
	rows, err := s.proxyStateDB().Query(
		`SELECT key FROM proxy_state WHERE substr(key, 1, ?) = ?`, len(prefix), prefix)
	if err != nil {
		return 0, err
	}
	var stale []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, err
		}
		if !keep[key] {
			stale = append(stale, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var deleted int64
	for _, key := range stale {
		res, err := s.proxyStateDB().Exec(`DELETE FROM proxy_state WHERE key = ?`, key)
		if err != nil {
			return deleted, err
		}
		if n, err := res.RowsAffected(); err == nil {
			deleted += n
		}
	}
	return deleted, nil
}

// DeleteProxyStatePrefixOlderThan deletes every entry under prefix whose
// updated_at lies more than age in the past. Prefix matching uses substr, not
// LIKE, because the key space is written by clients (see PruneProxyStatePrefix).
//
// The comparison runs through julianday so both CURRENT_TIMESTAMP rows and
// RFC3339 rows are dated correctly. A value julianday cannot read yields NULL,
// which the comparison treats as false: a row of unknown age is kept, never
// deleted.
func (s *Store) DeleteProxyStatePrefixOlderThan(prefix string, age time.Duration) (int, error) {
	if prefix == "" || age <= 0 {
		return 0, nil
	}
	cutoff := fmt.Sprintf("-%d seconds", int(age.Seconds()))
	res, err := s.proxyStateDB().Exec(
		`DELETE FROM proxy_state WHERE substr(key, 1, ?) = ? AND julianday(updated_at) < julianday('now', ?)`,
		len(prefix), prefix, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// GetProxyState retrieves a value from proxy_state. Returns "" if not found.
func (s *Store) GetProxyState(key string) (string, error) {
	var val string
	err := s.proxyStateDB().QueryRow(`SELECT value FROM proxy_state WHERE key = ?`, key).Scan(&val)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return val, nil
}

// DeleteProxyState removes one exact proxy_state entry.
func (s *Store) DeleteProxyState(key string) error {
	_, err := s.proxyStateDB().Exec(`DELETE FROM proxy_state WHERE key = ?`, key)
	return err
}

// DeleteProxyStatePrefix deletes all proxy_state entries whose key starts with prefix.
// Prefix matching uses instr, not LIKE, for the same reason as the sibling
// functions: a client-written key may contain _ or %, which LIKE would read as
// wildcards and so delete rows outside the prefix. Matching is exact, so unlike
// LIKE it is case-sensitive, and instr keeps it character-based for a multi-byte
// prefix (a substr with a byte length would find nothing). An empty prefix
// matches nothing and deletes nothing.
// Returns the number of rows deleted.
func (s *Store) DeleteProxyStatePrefix(prefix string) (int, error) {
	if prefix == "" {
		return 0, nil
	}
	result, err := s.proxyStateDB().Exec(
		`DELETE FROM proxy_state WHERE instr(key, ?) = 1`, prefix)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// GetLastCheckedCommit returns the last commit hash processed for staleness scanning.
func (s *Store) GetLastCheckedCommit(project string) (string, error) {
	return s.GetProxyState("staleness_last_checked:" + project)
}

// SetLastCheckedCommit stores the last commit hash processed for staleness scanning.
func (s *Store) SetLastCheckedCommit(project, hash string) error {
	return s.SetProxyState("staleness_last_checked:"+project, hash)
}

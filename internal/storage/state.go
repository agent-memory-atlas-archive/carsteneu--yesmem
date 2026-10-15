package storage

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// GetIndexState returns the index state for a JSONL path, or nil if not indexed.
func (s *Store) GetIndexState(jsonlPath string) (*models.IndexState, error) {
	row := s.readerDB().QueryRow(`SELECT jsonl_path, file_size, file_mtime, indexed_at,
		indexed_offset, indexed_session_id, indexed_msg_count, indexed_prefix_hash
		FROM index_state WHERE jsonl_path = ?`, jsonlPath)

	st := &models.IndexState{}
	var mtime, indexedAt string
	err := row.Scan(&st.JSONLPath, &st.FileSize, &mtime, &indexedAt,
		&st.IndexedOffset, &st.IndexedSessionID, &st.IndexedMsgCount, &st.IndexedPrefixHash)
	if err != nil {
		return nil, err
	}
	st.FileMtime = parseTime(mtime)
	st.IndexedAt = parseTime(indexedAt)
	return st, nil
}

// UpsertIndexState records that a JSONL file has been indexed.
func (s *Store) UpsertIndexState(st *models.IndexState) error {
	_, err := s.db.Exec(`INSERT INTO index_state (jsonl_path, file_size, file_mtime, indexed_at,
		indexed_offset, indexed_session_id, indexed_msg_count, indexed_prefix_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(jsonl_path) DO UPDATE SET
			file_size = excluded.file_size,
			file_mtime = excluded.file_mtime,
			indexed_at = excluded.indexed_at,
			indexed_offset = excluded.indexed_offset,
			indexed_session_id = excluded.indexed_session_id,
			indexed_msg_count = excluded.indexed_msg_count,
			indexed_prefix_hash = excluded.indexed_prefix_hash`,
		st.JSONLPath, st.FileSize, fmtTime(st.FileMtime), fmtTime(st.IndexedAt),
		st.IndexedOffset, st.IndexedSessionID, st.IndexedMsgCount, st.IndexedPrefixHash)
	return err
}

// NeedsReindex checks if a file needs re-indexing by comparing size and mtime.
// Truncates mtime to seconds because RFC3339 storage loses nanoseconds.
func (s *Store) NeedsReindex(jsonlPath string, size int64, mtime time.Time) bool {
	st, err := s.GetIndexState(jsonlPath)
	if err != nil {
		return true // not indexed yet
	}
	return st.FileSize != size || !st.FileMtime.Truncate(time.Second).Equal(mtime.Truncate(time.Second))
}

// SessionIndexedByOtherPath reports whether a session id is also recorded
// against a different JSONL path. Two files can carry the same session id (a
// resumed session keeps its parent id behind a snapshot header, and a copied or
// restored file duplicates the id), so only the file that owns a session may
// take an append fast path: appending would otherwise trim messages that the
// other file contributed.
//
// A row whose file is not there owns nothing: it cannot contribute rows now, and
// while it is counted the session's surviving file is kept out of the fast path
// for good (a file that Claude Code's 30-day cleanup deleted leaves such a row
// behind). It is dropped rather than merely ignored, so the recorded state stays
// honest and a file that comes back is re-indexed from scratch instead of being
// trusted against a watermark that nothing has verified since.
func (s *Store) SessionIndexedByOtherPath(sessionID, jsonlPath string) (bool, error) {
	rows, err := s.readerDB().Query(
		`SELECT jsonl_path FROM index_state WHERE indexed_session_id = ? AND jsonl_path <> ?`,
		sessionID, jsonlPath)
	if err != nil {
		return false, fmt.Errorf("check session ownership of %s: %w", sessionID, err)
	}

	// Collect first: the deletion below must not run while the cursor over the
	// same table is open (in :memory: mode both are the same database).
	var gone []string
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			rows.Close()
			return false, fmt.Errorf("check session ownership of %s: %w", sessionID, err)
		}
		if _, err := os.Stat(other); err == nil {
			rows.Close()
			return true, nil
		}
		gone = append(gone, other)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("check session ownership of %s: %w", sessionID, err)
	}
	rows.Close()

	for _, other := range gone {
		if _, err := s.db.Exec(
			`DELETE FROM index_state WHERE jsonl_path = ? AND indexed_session_id = ?`,
			other, sessionID); err != nil {
			return false, fmt.Errorf("drop stale index state for %s: %w", other, err)
		}
		log.Printf("index_state: dropped the row for %s (file no longer exists), session %s", other, sessionID)
	}
	return false, nil
}

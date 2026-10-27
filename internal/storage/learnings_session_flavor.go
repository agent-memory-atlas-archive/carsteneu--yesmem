package storage

import (
	"fmt"
	"time"
)

// GetSessionFlavorsForSession returns all distinct flavors for a single session, chronologically.
// Unlike GetSessionFlavorsSince (which groups by session_id for multi-session overview),
// this returns all phase flavors within one session to show its evolution.
func (s *Store) GetSessionFlavorsForSession(sessionID string) ([]map[string]any, error) {
	query := `SELECT session_flavor, MIN(created_at) as first_seen
		FROM learnings
		WHERE superseded_by IS NULL
		AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))
		AND session_id = ?
		AND length(session_flavor) > 0
		GROUP BY session_flavor
		ORDER BY first_seen ASC`

	rows, err := s.readerDB().Query(query, fmtTime(time.Now()), sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session flavors for session: %w", err)
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var flavor, createdAt string
		if err := rows.Scan(&flavor, &createdAt); err != nil {
			continue
		}
		results = append(results, map[string]any{
			"session_flavor": flavor,
			"created_at":     createdAt,
			"session_id":     sessionID,
		})
	}
	return results, nil
}

// UpdateSessionFlavorOnlyEmpty sets session_flavor only on learnings that don't already have one.
// This preserves earlier phase flavors when extraction runs multiple times on long sessions.
// Also stores the learnings_count on the session for flavor grounding checks.
func (s *Store) UpdateSessionFlavorOnlyEmpty(sessionID, flavor string, learningsCount int64) (int64, error) {
	result, err := s.db.Exec(`UPDATE learnings SET session_flavor = ?
		WHERE session_id = ? AND superseded_by IS NULL
		AND (session_flavor = '' OR session_flavor IS NULL)`, flavor, sessionID)
	if err != nil {
		return 0, fmt.Errorf("update session flavor only empty: %w", err)
	}

	// Persist learnings count on session for grounding checks
	if learningsCount >= 0 {
		if _, err := s.db.Exec(`UPDATE sessions SET flavor_learnings_count = ? WHERE id = ?`, learningsCount, sessionID); err != nil {
			return 0, fmt.Errorf("persist flavor_learnings_count for session %s: %w", sessionID, err)
		}
	}

	return result.RowsAffected()
}

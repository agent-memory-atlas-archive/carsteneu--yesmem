package storage

import (
	"database/sql"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// markFilter excludes daemon-generated artifacts from mark counting: the
// narrative/pulse learnings exist for every extracted session. Counting them
// would defeat the confabulation gate (#72444: sessions without real marks
// must keep flavor_learnings_count = 0) and fake marks in the briefing.
const markFilter = `AND category NOT IN ('narrative', 'pulse')`

// CountLearningsForSession returns how many learnings a session left behind
// ("marks"). The briefing state-letter composer needs this to distinguish
// sessions that carried insights from sessions that only passed through.
func (s *Store) CountLearningsForSession(sessionID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM learnings WHERE session_id = ? `+markFilter, sessionID,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// GetLearningsCounts bulk-loads mark counts for a set of sessions
// (session_id → count). Sessions without learnings are absent from the map.
func (s *Store) GetLearningsCounts(sessionIDs []string) (map[string]int, error) {
	counts := make(map[string]int, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return counts, nil
	}
	placeholders := ""
	args := make([]any, 0, len(sessionIDs))
	for i, id := range sessionIDs {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	rows, err := s.db.Query(
		`SELECT session_id, COUNT(*) FROM learnings WHERE session_id IN (`+placeholders+`) `+markFilter+` GROUP BY session_id`, args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var n int
		if err := rows.Scan(&sid, &n); err != nil {
			return nil, err
		}
		counts[sid] = n
	}
	return counts, rows.Err()
}

// GetLatestStateBrief resolves the Zustandsbrief shown in a project's
// briefing: the project's own active head (category=strategic, not
// superseded, brief marker prefix) wins; without an active local brief the
// newest active brief of any project is the fallback. Superseding is LOCAL:
// a new brief supersedes the previous head of its own project only — the
// global fallback head of another project is never overwritten by it.
func (s *Store) GetLatestStateBrief(projectShort string) (*models.Learning, error) {
	const filter = `category = 'strategic'
			AND superseded_by IS NULL
			AND (content LIKE 'State brief%' OR content LIKE 'STATE LETTER%' OR content LIKE 'Zustandsbrief%')`

	canonical := s.resolveCanonicalProject(projectShort)
	row := s.db.QueryRow(`
		SELECT id, content, created_at FROM learnings
		WHERE `+filter+` AND canonical_project = ?
		ORDER BY id DESC LIMIT 1`, canonical)

	var l models.Learning
	var created string
	err := row.Scan(&l.ID, &l.Content, &created)
	if err == nil {
		if t, perr := time.Parse(time.RFC3339, created); perr == nil {
			l.CreatedAt = t
		}
		return &l, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	// No active local brief — fall back to the newest active brief overall.
	row = s.db.QueryRow(`
		SELECT id, content, created_at FROM learnings
		WHERE `+filter+`
		ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&l.ID, &l.Content, &created); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if t, perr := time.Parse(time.RFC3339, created); perr == nil {
		l.CreatedAt = t
	}
	return &l, nil
}

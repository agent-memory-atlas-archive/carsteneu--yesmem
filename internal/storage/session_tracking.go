package storage

import (
	"database/sql"
	"errors"
	"time"
)

// TrackSessionEnd records that a session ended (clear/logout/...). A repeated
// end for the same session updates the row but keeps its consumption: a hook
// that delivers the same SessionEnd twice must not re-open a handoff that a
// briefing already used.
func (s *Store) TrackSessionEnd(project, sessionID, reason string) error {
	_, err := s.db.Exec(`INSERT INTO session_tracking (project, session_id, reason, timestamp)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(project, session_id) DO UPDATE SET
			reason = excluded.reason,
			timestamp = excluded.timestamp`, project, sessionID, reason, time.Now().UTC().Format(time.RFC3339))
	return err
}

// GetPendingRecovery returns the most recent ended session that no briefing has
// consumed yet, together with its reason, and marks it consumed.
//
// The handoff is one-shot rather than time-bound: the first briefing after a
// /clear uses it, whether the user types the next prompt five seconds or five
// minutes later. The 30-second window this replaces dropped the recovery
// whenever the user paused. maxAge only bounds how old an unconsumed entry may
// be, so a clear whose follow-up session never started cannot surface as fresh
// context weeks later.
//
// Consumption is decided by the UPDATE, not by the preceding read: the write
// carries the whole "still unconsumed" condition and reports whether it hit
// anything, so two briefings racing for the same project cannot both receive
// the handoff. The same statement sweeps every older unconsumed row of the
// project, because rows left behind — from before this column existed, or from
// a clear whose follow-up never came — would otherwise be delivered one per
// briefing, newest first.
func (s *Store) GetPendingRecovery(project string, maxAge time.Duration) (string, string, error) {
	// The marker identifies the rows this call consumed, so the read-back below
	// cannot pick up another caller's row. A fixed fraction width keeps it apart
	// from the second-resolution timestamps older rows carry, and per call it is
	// unique in anything but name.
	marker := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	res, err := s.db.Exec(`UPDATE session_tracking SET consumed_at = ?
		WHERE project = ? AND reason = 'clear' AND consumed_at IS NULL`, marker, project)
	if err != nil {
		return "", "", err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		// Another briefing took the handoff first.
		return "", "", err
	}

	var sid, reason, ts string
	err = s.db.QueryRow(`SELECT session_id, reason, timestamp FROM session_tracking
		WHERE project = ? AND reason = 'clear' AND consumed_at = ?
		ORDER BY timestamp DESC, rowid DESC LIMIT 1`, project, marker).Scan(&sid, &reason, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}

	t, err := time.Parse(time.RFC3339, ts)
	if err != nil || time.Since(t) > maxAge {
		return "", "", nil
	}
	return sid, reason, nil
}

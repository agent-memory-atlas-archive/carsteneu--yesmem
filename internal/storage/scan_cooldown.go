package storage

import (
	"database/sql"
	"time"
)

// RecordScanFailure persists a scan failure timestamp for a project.
// Used to suppress retry storms: while the cooldown is active, scans for
// the project are skipped instead of re-running a bound slow scan.
func (s *Store) RecordScanFailure(project, errMsg string) error {
	_, err := s.db.Exec(`INSERT INTO scan_cooldown (project, failed_at, error)
		VALUES (?, ?, ?)
		ON CONFLICT(project) DO UPDATE SET failed_at=excluded.failed_at, error=excluded.error`,
		project, time.Now().Unix(), errMsg)
	return err
}

// GetScanFailure returns the last failure for a project, or zero time if none.
func (s *Store) GetScanFailure(project string) (time.Time, string, error) {
	row := s.db.QueryRow(`SELECT failed_at, error FROM scan_cooldown WHERE project = ?`, project)
	var failedAt int64
	var errMsg string
	err := row.Scan(&failedAt, &errMsg)
	if err == sql.ErrNoRows {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	return time.Unix(failedAt, 0), errMsg, nil
}

// ClearScanFailure removes the failure record for a project.
func (s *Store) ClearScanFailure(project string) error {
	_, err := s.db.Exec(`DELETE FROM scan_cooldown WHERE project = ?`, project)
	return err
}

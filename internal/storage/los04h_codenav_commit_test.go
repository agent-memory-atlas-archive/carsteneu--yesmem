package storage

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// A COMMIT cannot be made to fail through the public API without manipulating
// the connection, so the commit branch of the backfill had no repro (YM-366).
// commitScanFilesTx is the seam: a failed commit must leave no path index behind
// and must say where it failed.
func TestBackfillScanFiles_SurvivesAFailedCommit(t *testing.T) {
	s := mustOpen(t)
	project := "los04h-commit"
	scanJSON := `{"Packages":[{"Files":[{"Path":"b.go"}]}]}`
	if _, err := s.db.Exec(
		`INSERT INTO project_scan (project, scan_json, git_head) VALUES (?, ?, 'head')`,
		project, scanJSON); err != nil {
		t.Fatalf("seed project_scan: %v", err)
	}

	previous := commitScanFilesTx
	commitScanFilesTx = func(*sql.Tx) error { return errors.New("commit refused for the test") }
	t.Cleanup(func() { commitScanFilesTx = previous })

	logged := los04gCaptureLog(t)
	if s.IsFileInCodeIndex(project, "b.go") {
		t.Fatalf("index lookup must miss after a failed commit; log: %s", logged.String())
	}

	// The rewrite was rolled back: a half-written index must not survive a failed
	// commit, or every later lookup would trust it.
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_scan_files WHERE project = ?`, project).Scan(&rows); err != nil {
		t.Fatalf("count index rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a failed commit left %d index rows behind", rows)
	}

	got := logged.String()
	if !strings.Contains(got, "commit refused for the test") {
		t.Fatalf("the reason of the failed commit was dropped: %q", got)
	}
	// The commit branch used to share its text with the rewrite branch; the log
	// has to name the step that failed.
	if !strings.Contains(got, "commit failed") {
		t.Fatalf("the log does not name the failed commit: %q", got)
	}
}

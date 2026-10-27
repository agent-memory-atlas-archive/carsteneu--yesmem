package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// The v0.65 backfill rewrites learnings.project from the authoritative session
// project. opencode writes carry session ids that never appear in the sessions
// table (the sessions index only holds indexed Claude-style transcripts), and
// for those the subquery yields NULL — so every store open used to blank the
// project of a learning whose session is unknown. The whole migrations list is
// re-run on every open (createSchema), so the damage repeated at every start.

const v065SeedContent = "v0.65 regression seed"

func seedProjectRow(t *testing.T, s *Store, project, sessionID string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO learnings (content, category, project, session_id, created_at, model_used)
		 VALUES (?, 'gotcha', ?, ?, '2026-10-01T00:00:00+02:00', 'test')`,
		v065SeedContent, project, sessionID,
	); err != nil {
		t.Fatalf("seed learning: %v", err)
	}
}

func readProject(t *testing.T, s *Store) sql.NullString {
	t.Helper()
	var project sql.NullString
	if err := s.db.QueryRow(`SELECT project FROM learnings WHERE content = ?`, v065SeedContent).Scan(&project); err != nil {
		t.Fatalf("read back: %v", err)
	}
	return project
}

// A session that is not in the sessions table must leave the project alone.
func TestV065ProjectBackfillKeepsUnresolvedProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	seedProjectRow(t, s, "wal", "opencode:ses_absent_from_sessions")
	s.Close()

	// A second open re-runs the migration list, exactly as the daemon does on
	// every start.
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	if project := readProject(t, again); !project.Valid || project.String != "wal" {
		t.Fatalf("project = %q (valid=%v), want \"wal\" — an unresolvable session must not blank the project",
			project.String, project.Valid)
	}
}

// A matching session still backfills the full path; the guard must not switch
// off the migration it protects.
func TestV065ProjectBackfillStillResolvesMatchingSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const sid = "opencode:ses_indexed"
	if err := s.UpsertSession(&models.Session{ID: sid, Project: "/repo/main", StartedAt: time.Now()}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	seedProjectRow(t, s, "main", sid)
	s.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	if project := readProject(t, again); !project.Valid || project.String != "/repo/main" {
		t.Fatalf("project = %q (valid=%v), want the session's full path", project.String, project.Valid)
	}
}

// A session whose project is itself a short name must not overwrite a project
// with something that is not a path.
func TestV065ProjectBackfillIgnoresNonPathSessionProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const sid = "opencode:ses_short"
	if err := s.UpsertSession(&models.Session{ID: sid, Project: "main", StartedAt: time.Now()}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	seedProjectRow(t, s, "wal", sid)
	s.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	if project := readProject(t, again); !project.Valid || project.String != "wal" {
		t.Fatalf("project = %q (valid=%v), want \"wal\" — a non-path session project is no improvement",
			project.String, project.Valid)
	}
}

// canonical_project holds the project *basename* (models.CanonicalProject),
// while project holds the full path. The retired v0.61 backfill
// `UPDATE learnings SET canonical_project = project` therefore wrote a path into
// a basename column, and because the list re-runs on every open it did so to
// every row that still had an empty canonical — 1110 rows on the live database
// when it was removed. This guards that it stays removed.
func TestStoreOpenKeepsCanonicalProjectBasename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO learnings (content, category, project, canonical_project, created_at, model_used)
		 VALUES ('canonical seed', 'gotcha', '/repo/wal', 'wal', '2026-10-01T00:00:00+02:00', 'test')`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	var canonical string
	if err := again.db.QueryRow(`SELECT canonical_project FROM learnings WHERE content = 'canonical seed'`).Scan(&canonical); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if canonical != "wal" {
		t.Fatalf("canonical_project = %q, want the basename \"wal\" — a store open must not overwrite it with the full path", canonical)
	}
}

package storage

// Shared fixtures for the storage package. One opener per storage shape, so a
// change to how tests build a store touches this file instead of a hundred.
//
// The tier names keep the ticket ids out of the helpers: the los/ym id lives on
// in the test function that exercises the behaviour, which is where a reader
// looks for it.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// mustOpen returns a fresh in-memory store. In :memory: mode all logical
// databases (yesmem/messages/runtime) share one handle, so index_state and
// messages live in the same schema. Good enough for storage-level tests, and
// the default unless a test needs the real file split.
func mustOpen(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mustOpenFiles opens a file-backed store: session pins live in runtime.db and
// permanent pins in yesmem.db, two separate files as in production. With
// ":memory:" both scopes share one database and a clear would wipe both. Also
// the shape to pick when a test would deadlock on the single connection of an
// in-memory database (a cursor held open across a second query).
func mustOpenFiles(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mustOpenWithPath is mustOpenFiles for tests that need the database path
// themselves: reopen assertions, WAL frame inspection, journal size limits.
func mustOpenWithPath(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yesmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store at %s: %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// mustOpenCapStore is mustOpen plus an attached cap_store database.
func mustOpenCapStore(t *testing.T) *Store {
	t.Helper()
	s := mustOpen(t)
	if err := s.OpenCapsDB(t.TempDir()); err != nil {
		t.Fatalf("open cap_store: %v", err)
	}
	t.Cleanup(func() { s.CloseCapsDB() })
	return s
}

// seedSession registers a session the way the indexer does: project carries the
// absolute path, UpsertSession normalizes project_short from it, and StartedAt is
// set. A fixture that cares about session ordering must still set its own
// timestamp, because started_at only decides MAX(started_at) tie-breaks.
func seedSession(t *testing.T, s *Store, id, project string) {
	t.Helper()
	if err := s.UpsertSession(&models.Session{ID: id, Project: project, StartedAt: time.Now()}); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

// seedPin inserts a pin with an explicit creation time, so age-based behaviour
// can be tested without waiting.
func seedPin(t *testing.T, s *Store, scope, project, sessionID, content string, createdAt time.Time) int64 {
	t.Helper()
	res, err := s.pinnedDB(scope).Exec(
		`INSERT INTO pinned_learnings (project, session_id, content, source, created_at) VALUES (?, ?, ?, 'test', ?)`,
		project, sessionID, content, createdAt.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatalf("seed pin %q: %v", content, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("pin id of %q: %v", content, err)
	}
	return id
}

// seedPinNow is seedPin for the common case: no session binding, created now.
func seedPinNow(t *testing.T, s *Store, scope, project, content string) int64 {
	t.Helper()
	return seedPin(t, s, scope, project, "", content, time.Now())
}

// pinContents returns the content of every pin the given scope exposes for the
// given project.
func pinContents(t *testing.T, s *Store, scope, project string) []string {
	t.Helper()
	pins, err := s.GetPinnedLearnings(scope, project)
	if err != nil {
		t.Fatalf("get %s pins for %q: %v", scope, project, err)
	}
	out := make([]string, 0, len(pins))
	for _, p := range pins {
		out = append(out, p.Content)
	}
	return out
}

func pinHasContent(contents []string, want string) bool {
	for _, c := range contents {
		if c == want {
			return true
		}
	}
	return false
}

// countProxyStateRows counts rows by exact key: GetProxyState returns "" both
// for a deleted row and for an emptied one, so it cannot prove a DELETE.
func (s *Store) countProxyStateRows(t *testing.T, key string) int {
	t.Helper()
	var n int
	if err := s.proxyStateDB().QueryRow(`SELECT count(*) FROM proxy_state WHERE key = ?`, key).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", key, err)
	}
	return n
}

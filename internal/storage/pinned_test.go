package storage

import (
	"path/filepath"
	"testing"
)

// YM-227: a project-less session pin belongs to the session that created it.
// Clearing another session's project must not remove it — runtime.db is one
// file shared by every project and session.
func TestClearSessionPins_LeavesOtherSessionsProjectlessPin(t *testing.T) {
	s := mustOpenFiles(t)

	if _, err := s.PinLearning("session", "", "pin from sess-a", "test", "sess-a"); err != nil {
		t.Fatalf("pin a: %v", err)
	}
	if _, err := s.PinLearning("session", "", "pin from sess-b", "test", "sess-b"); err != nil {
		t.Fatalf("pin b: %v", err)
	}
	if _, err := s.PinLearning("session", "/home/testuser/projects/myapp", "project pin", "test", "sess-b"); err != nil {
		t.Fatalf("project pin: %v", err)
	}

	if err := s.ClearSessionPins("/home/testuser/projects/myapp", "sess-b"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	pins, err := s.GetPinnedLearnings("session", "/home/testuser/projects/myapp")
	if err != nil {
		t.Fatalf("get pins: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("expected only the foreign session's project-less pin to survive, got %d pins", len(pins))
	}
	if pins[0].Content != "pin from sess-a" {
		t.Errorf("surviving pin = %q, want the sess-a pin", pins[0].Content)
	}
}

func TestClearSessionPins_RemovesOwnProjectlessPin(t *testing.T) {
	s := mustOpenFiles(t)

	if _, err := s.PinLearning("session", "", "own pin", "test", "sess-b"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := s.PinLearning("permanent", "", "durable pin", "test", "sess-b"); err != nil {
		t.Fatalf("permanent pin: %v", err)
	}

	if err := s.ClearSessionPins("/home/testuser/projects/myapp", "sess-b"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	sessionPins, _ := s.GetPinnedLearnings("session", "/home/testuser/projects/myapp")
	if len(sessionPins) != 0 {
		t.Errorf("the ending session's project-less pin must be cleared, got %d", len(sessionPins))
	}
	permanentPins, _ := s.GetPinnedLearnings("permanent", "/home/testuser/projects/myapp")
	if len(permanentPins) != 1 {
		t.Errorf("permanent pins must survive /clear, got %d", len(permanentPins))
	}
}

func TestPinLearning_RecordsSessionID(t *testing.T) {
	s := mustOpen(t)

	id, err := s.PinLearning("session", "", "attributed pin", "test", "sess-a")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}

	var got string
	if err := s.proxyStateDB().QueryRow(`SELECT session_id FROM pinned_learnings WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("read session_id: %v", err)
	}
	if got != "sess-a" {
		t.Errorf("session_id = %q, want sess-a", got)
	}
}

// YM-227: an existing installation carries pinned_learnings without session_id
// in BOTH databases. Open must add the column to both, otherwise permanent pins
// (yesmem.db) fail while session pins (runtime.db) work.
func TestOpenAddsSessionIDToPreMigrationPinnedTables(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "yesmem.db")
	runtimePath := filepath.Join(dir, "runtime.db")
	legacyDDL := `CREATE TABLE pinned_learnings (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		project    TEXT NOT NULL DEFAULT '',
		content    TEXT NOT NULL,
		source     TEXT NOT NULL DEFAULT 'user_stated',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`
	for _, p := range []string{dbPath, runtimePath} {
		db, err := openSQLite(p)
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		if _, err := db.Exec(legacyDDL); err != nil {
			db.Close()
			t.Fatalf("create legacy table in %s: %v", p, err)
		}
		if _, err := db.Exec(`INSERT INTO pinned_learnings (project, content, source) VALUES ('', 'legacy pin', 'user_stated')`); err != nil {
			db.Close()
			t.Fatalf("seed legacy pin in %s: %v", p, err)
		}
		db.Close()
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if _, err := s.PinLearning("permanent", "/home/testuser/projects/myapp", "perm", "test", "sess-a"); err != nil {
		t.Fatalf("permanent pin after upgrade: %v", err)
	}
	if _, err := s.PinLearning("session", "/home/testuser/projects/myapp", "sess", "test", "sess-a"); err != nil {
		t.Fatalf("session pin after upgrade: %v", err)
	}

	pins, err := s.GetPinnedLearnings("permanent", "/home/testuser/projects/myapp")
	if err != nil {
		t.Fatalf("get permanent pins: %v", err)
	}
	if len(pins) != 2 {
		t.Errorf("expected the legacy pin plus the new permanent pin, got %d", len(pins))
	}
}

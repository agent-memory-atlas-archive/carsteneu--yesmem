package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// yesmem.db holds learnings, persona traits and session content, runtime.db and
// messages.db their own slices of it. Open used to let SQLite create them with
// the umask, so on a host with umask 022 the whole knowledge base was
// world-readable in a 0755 directory (audit 16.10.2026, YM-328). Only the owner
// may read them; every client runs as the same user.
func TestOpen_HardensDatabaseFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yesmem.db")

	// An existing installation: the file is already there with 0644.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed existing db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for _, p := range []string{path, filepath.Join(dir, "runtime.db"), filepath.Join(dir, "messages.db")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", filepath.Base(p), got)
		}
	}

	// The -wal and -shm siblings carry the most recent commits and inherit the
	// mode the database had when the connection opened them.
	for _, p := range []string{path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", filepath.Base(p), got)
		}
	}
}

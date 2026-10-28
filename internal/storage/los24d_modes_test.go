package storage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// YM-338 (Los 24d): openSQLite hardens yesmem.db, runtime.db and messages.db, but
// the data directory also holds databases that never pass through it
// (guard_state.db, caps.db, and the empty legacy stubs agents.db,
// sessions.db, learnings.db, proxy_rt.db). They kept the umask mode.
func TestHardenDBFilesInDirCoversEveryDatabaseAndSidecar(t *testing.T) {
	dir := t.TempDir()
	databases := []string{"guard_state.db", "cap_store.db", "agents.db", "yesmem.db-wal", "runtime.db-shm", "yesmem.db.bak-pre-apply"}
	for _, name := range databases {
		writeWithMode(t, filepath.Join(dir, name), 0o644)
	}
	other := filepath.Join(dir, "notes.txt")
	writeWithMode(t, other, 0o644)
	// A directory whose name ends in .db must be left alone: chmod on it would
	// change how the directory is reachable, not how a file is readable.
	nested := filepath.Join(dir, "sub.db")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(nested, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	hardened, err := HardenDBFilesInDir(dir)
	if err != nil {
		t.Fatalf("harden: %v", err)
	}
	if hardened != len(databases) {
		t.Errorf("hardened %d files, want %d", hardened, len(databases))
	}
	for _, name := range databases {
		mode := modeOf(t, filepath.Join(dir, name))
		if mode&0o077 != 0 {
			t.Errorf("%s is %04o, want owner-only", name, mode)
		}
	}
	if mode := modeOf(t, other); mode != 0o644 {
		t.Errorf("notes.txt became %04o, want unchanged 0644", mode)
	}
	if mode := modeOf(t, nested); mode != 0o755 {
		t.Errorf("sub.db directory became %04o, want unchanged 0755", mode)
	}
}

// A data directory that does not exist yet is not an error: the daemon creates
// it later, and the sweep runs before the first open.
func TestHardenDBFilesInDirIgnoresAMissingDirectory(t *testing.T) {
	hardened, err := HardenDBFilesInDir(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("harden: %v", err)
	}
	if hardened != 0 {
		t.Errorf("hardened %d files in a missing directory", hardened)
	}
}

// YM-338: a filesystem that cannot chmod (some FUSE and network mounts) made the
// daemon unstartable because a failed chmod was a hard error. The data directory
// itself is 0700, so a mode that cannot be set is a warning, not a lost start.
func TestOpenSQLiteSurvivesAFileSystemWithoutChmod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")

	previous := chmodFile
	chmodFile = func(string, os.FileMode) error { return syscall.EPERM }
	t.Cleanup(func() { chmodFile = previous })

	db, err := openSQLite(path)
	if err != nil {
		t.Fatalf("openSQLite on a filesystem without chmod: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("database is unusable: %v", err)
	}
}

func writeWithMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

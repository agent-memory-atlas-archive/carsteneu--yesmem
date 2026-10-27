package hooks

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// YM-338 (Los 24d): guard_state.db is created by the short-lived hook process,
// not by the daemon, so the daemon's startup sweep never sees its first version.
// The cooldown database records which skills were suggested and is as private as
// the rest of the data directory.
func TestCooldownDatabaseIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard_state.db")

	previous := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(previous) })

	db, err := openCooldownDB(path)
	if err != nil {
		t.Fatalf("open cooldown db: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO guard_cooldown (skill, last_fired_at) VALUES ('x', 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue // a sidecar that was never created needs no mode
		}
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s is %04o, want owner-only", filepath.Base(p), mode)
		}
	}
}

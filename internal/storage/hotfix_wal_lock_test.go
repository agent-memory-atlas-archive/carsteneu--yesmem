package storage

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// processHoldsLockOn reports whether /proc/locks lists a lock owned by this
// process on the file at path.
func processHoldsLockOn(t *testing.T, path string) bool {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	f, err := os.Open("/proc/locks")
	if err != nil {
		t.Skipf("no /proc/locks: %v", err)
	}
	defer f.Close()
	pid := fmt.Sprint(os.Getpid())
	suffix := fmt.Sprintf(":%d", st.Ino)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// "1: POSIX  ADVISORY  READ 1234 fd:00:5678 0 EOF"
		for i, fld := range fields {
			if fld == pid && i+1 < len(fields) && strings.HasSuffix(fields[i+1], suffix) {
				return true
			}
		}
	}
	return false
}

// A second openSQLite on a database this process already holds open must not
// cost the first connection its lock. Closing any descriptor of a file drops
// every POSIX lock the process holds on it, and SQLite does not notice: it
// counts the lock as held. The next process to close its own connection then
// takes itself for the last one and deletes -wal and -shm under the running
// daemon (live 18.10.2026: disk I/O error 522, daemon held no lock on
// yesmem.db and messages.db while runtime.db kept its lock).
func TestHotfixWAL_SecondOpenKeepsTheProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")

	first, err := openSQLite(path)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	defer first.Close()
	if _, err := first.Exec("CREATE TABLE t (v INTEGER); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !processHoldsLockOn(t, path) {
		t.Fatal("precondition: the first connection holds no lock on the database file")
	}

	second, err := openSQLite(path)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer second.Close()
	if _, err := second.Exec("SELECT count(*) FROM t"); err != nil {
		t.Fatalf("read second: %v", err)
	}

	if !processHoldsLockOn(t, path) {
		t.Fatal("the second openSQLite dropped this process's lock on the database file")
	}
}

// An existing database file is hardened by chmod alone, never opened: a file
// the owner may only read would make an O_RDWR open fail.
func TestHotfixWAL_HardenDoesNotOpenAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")
	if err := os.WriteFile(path, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := hardenDBFile(path); err != nil {
		t.Fatalf("harden existing file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

// A missing database file is still created owner-only.
func TestHotfixWAL_HardenCreatesAMissingFileOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yesmem.db")
	if err := hardenDBFile(path); err != nil {
		t.Fatalf("harden missing file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

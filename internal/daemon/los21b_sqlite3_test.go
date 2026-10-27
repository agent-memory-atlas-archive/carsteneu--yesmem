package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// los21bWriteFakeSQLite3 puts an executable named sqlite3 into a fresh directory
// and returns that directory. The fake prints a marker and exits.
func los21bWriteFakeSQLite3(t *testing.T, mode os.FileMode) (dir, marker string) {
	t.Helper()
	dir = t.TempDir()
	marker = "los21b-fake-sqlite3-ran"
	bin := filepath.Join(dir, "sqlite3")
	script := "#!/bin/sh\necho " + marker + "\n"
	if err := os.WriteFile(bin, []byte(script), mode); err != nil {
		t.Fatalf("write fake sqlite3: %v", err)
	}
	// WriteFile is subject to the umask; make the asserted mode explicit.
	if err := os.Chmod(bin, mode); err != nil {
		t.Fatalf("chmod fake sqlite3: %v", err)
	}
	return dir, marker
}

// YM-255: querySessionTables executed the bare name "sqlite3", so PATH decided
// which program ran with the daemon's privileges. A group- or world-writable
// launcher earlier in PATH is a binary anyone on the host may replace; the daemon
// must refuse it instead of running it.
func TestQuerySessionTables_RefusesWorldWritableSQLite3FromPath(t *testing.T) {
	dir, marker := los21bWriteFakeSQLite3(t, 0o777)
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(dbPath, []byte{}, 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	t.Setenv("PATH", dir)

	rows, err := querySessionTables(t.Context(), dbPath, func(table string) string {
		return "SELECT id FROM " + table
	})
	for _, row := range rows {
		if strings.Contains(row, marker) {
			t.Fatalf("the daemon executed a world-writable sqlite3 from PATH (output %q)", row)
		}
	}
	if err == nil {
		t.Fatalf("query with a world-writable sqlite3 returned rows=%v and no error", rows)
	}
	if !strings.Contains(err.Error(), "writable") {
		t.Errorf("error = %q, want it to name the writability problem", err)
	}
}

// A private executable (0755) is what a package manager installs; the resolver
// must keep using it, so the hardening cannot silently disable session capture.
func TestQuerySessionTables_UsesPrivateSQLite3FromPath(t *testing.T) {
	dir, marker := los21bWriteFakeSQLite3(t, 0o755)
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(dbPath, []byte{}, 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	t.Setenv("PATH", dir)

	rows, err := querySessionTables(t.Context(), dbPath, func(table string) string {
		return "SELECT id FROM " + table
	})
	if err != nil {
		t.Fatalf("a 0755 sqlite3 was refused: %v", err)
	}
	if len(rows) == 0 || rows[0] != marker {
		t.Fatalf("rows = %v, want the fake's output %q (the resolver stopped using a legitimate binary)", rows, marker)
	}
}

// Package managers install the CLI as a symlink; following it is part of the
// contract, not an accident, so a link to a private executable must pass.
func TestQuerySessionTables_FollowsSymlinkedSQLite3(t *testing.T) {
	dir, marker := los21bWriteFakeSQLite3(t, 0o755)
	linkDir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "sqlite3"), filepath.Join(linkDir, "sqlite3")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(dbPath, []byte{}, 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	t.Setenv("PATH", linkDir)

	rows, err := querySessionTables(t.Context(), dbPath, func(table string) string {
		return "SELECT id FROM " + table
	})
	if err != nil {
		t.Fatalf("a symlinked 0755 sqlite3 was refused: %v", err)
	}
	if len(rows) == 0 || rows[0] != marker {
		t.Fatalf("rows = %v, want the fake's output %q", rows, marker)
	}
}

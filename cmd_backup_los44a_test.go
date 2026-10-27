package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los44aInstallCapsDB adds a caps.db with the real schema and one marker row to
// the installation los38yInstall created.
func los44aInstallCapsDB(t *testing.T, dataDir string) {
	t.Helper()
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.OpenCapsDB(dataDir); err != nil {
		store.Close()
		t.Fatalf("create caps.db: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	los38ySeedDB(t, filepath.Join(dataDir, "caps.db"),
		`INSERT INTO cap_store_meta (cap_name, table_name, full_name)
			VALUES ('los44a', 'marker', 'cap_los44a__marker')`)
}

// TestRunBackupIncludesCapsDBLos44a is the regression test for OPS-95: the backup
// command copied yesmem.db, runtime.db and messages.db, but not caps.db, which
// holds every cap_store table. A restore from such a backup loses them.
func TestRunBackupIncludesCapsDBLos44a(t *testing.T) {
	dataDir, backupDir := los38yInstall(t)
	los44aInstallCapsDB(t, dataDir)

	los38yRunBackup(t, backupDir)

	files := los38yBackupFiles(t, backupDir)
	caps, ok := files["caps"]
	if !ok {
		t.Fatalf("backup holds no caps.db snapshot (found main=%q runtime=%q messages=%q) while %s/caps.db holds the cap_store tables; a restore would lose them",
			files["main"], files["runtime"], files["messages"], dataDir)
	}

	// A snapshot the operator cannot read is not a backup: open it and read the
	// marker row back.
	db, err := sql.Open("sqlite", caps)
	if err != nil {
		t.Fatalf("open caps snapshot: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cap_store_meta WHERE cap_name = 'los44a'`).Scan(&n); err != nil {
		t.Fatalf("read caps snapshot: %v", err)
	}
	if n != 1 {
		t.Fatalf("caps snapshot holds %d marker rows, want 1", n)
	}

	info, err := os.Stat(caps)
	if err != nil {
		t.Fatalf("stat caps snapshot: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("caps snapshot mode = %04o, want 0600: a backup holds full transcripts and cap tables", got)
	}
}

// TestRunBackupSkipsMissingCapsDBLos44a: a fresh installation creates caps.db on
// first use, so a backup without it has to succeed and say so instead of failing.
func TestRunBackupSkipsMissingCapsDBLos44a(t *testing.T) {
	dataDir, backupDir := los38yInstall(t)
	if _, err := os.Stat(filepath.Join(dataDir, "caps.db")); !os.IsNotExist(err) {
		t.Fatalf("fixture already holds a caps.db: %v", err)
	}

	los38yRunBackup(t, backupDir)

	files := los38yBackupFiles(t, backupDir)
	if _, ok := files["caps"]; ok {
		t.Fatalf("backup wrote a caps.db snapshot for an installation without caps.db")
	}
	if _, ok := files["main"]; !ok {
		t.Fatalf("backup holds no yesmem.db snapshot: %v", files)
	}
}

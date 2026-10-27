package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los38ySeedDB creates path with the given statements.
func los38ySeedDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec on %s: %v", path, err)
		}
	}
}

// los38yInstall builds a three-database installation under a temp HOME and returns
// the data directory and the backup directory the test writes into.
func los38yInstall(t *testing.T) (dataDir, backupDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir = filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	// The real schema comes from the store itself: a hand-written subset would
	// fail the migrations storage.Open runs on restore.
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	los38ySeedDB(t, filepath.Join(dataDir, "yesmem.db"),
		`INSERT INTO learnings (category, content, created_at, model_used)
			VALUES ('gotcha', 'backup marker', '2026-10-20T17:00:00Z', 'test')`)

	los38ySeedDB(t, filepath.Join(dataDir, "runtime.db"),
		`INSERT INTO proxy_state (key, value) VALUES ('marker', 'runtime')`)

	los38ySeedDB(t, filepath.Join(dataDir, "messages.db"),
		`INSERT INTO messages (session_id, role, message_type, content, tool_name, file_path, timestamp, sequence, model)
			VALUES ('ses-backup', 'user', 'text', 'the only copy of this message', '', '', '2026-10-20T17:00:00Z', 1, '')`)

	return dataDir, filepath.Join(home, "backup-out")
}

// los38yRunBackup runs the backup command against the temp HOME installation.
func los38yRunBackup(t *testing.T, backupDir string) {
	t.Helper()
	oldArgs := os.Args
	os.Args = []string{"yesmem", "backup", backupDir}
	t.Cleanup(func() { os.Args = oldArgs })
	runBackup()
}

// los38yBackupFiles maps a database role (main, runtime, messages) to the
// timestamped backup file the run wrote for it.
func los38yBackupFiles(t *testing.T, backupDir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	out := make(map[string]string, 4)
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, "-messages.db"):
			out["messages"] = filepath.Join(backupDir, name)
		case strings.HasSuffix(name, "-runtime.db"):
			out["runtime"] = filepath.Join(backupDir, name)
		case strings.HasSuffix(name, "-caps.db"):
			out["caps"] = filepath.Join(backupDir, name)
		case strings.HasSuffix(name, ".db"):
			out["main"] = filepath.Join(backupDir, name)
		}
	}
	return out
}

// TestRunBackupIncludesMessagesDBLos38y is the regression test for YM-369: the
// backup command copied yesmem.db and runtime.db, but not messages.db, which
// holds every message body since the migrate-messages split. A restore from such
// a backup loses the complete message history.
func TestRunBackupIncludesMessagesDBLos38y(t *testing.T) {
	dataDir, backupDir := los38yInstall(t)

	los38yRunBackup(t, backupDir)

	files := los38yBackupFiles(t, backupDir)
	if _, ok := files["messages"]; !ok {
		t.Fatalf("backup holds no messages.db snapshot (found main=%q runtime=%q) while %s/messages.db keeps every message body; a restore would lose the history",
			files["main"], files["runtime"], dataDir)
	}
	if _, ok := files["main"]; !ok {
		t.Fatalf("backup holds no yesmem.db snapshot: %v", files)
	}
}

// TestBackupRestoresMessagesLos38y restores a backup over the live files and
// reads the messages back through the real store, so the test proves the backup
// is usable, not merely present.
func TestBackupRestoresMessagesLos38y(t *testing.T) {
	dataDir, backupDir := los38yInstall(t)

	los38yRunBackup(t, backupDir)

	files := los38yBackupFiles(t, backupDir)
	messages, ok := files["messages"]
	if !ok {
		t.Fatalf("no messages.db snapshot to restore: %v", files)
	}

	// Restore: the backup files replace the live databases, as a manual restore does.
	for _, pair := range [][2]string{
		{files["main"], filepath.Join(dataDir, "yesmem.db")},
		{files["runtime"], filepath.Join(dataDir, "runtime.db")},
		{messages, filepath.Join(dataDir, "messages.db")},
	} {
		if pair[0] == "" {
			t.Fatalf("incomplete backup set, missing file for %s: %v", pair[1], files)
		}
		los38yReplaceFile(t, pair[0], pair[1])
	}

	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open restored store: %v", err)
	}
	defer store.Close()

	msgs, err := store.GetMessagesBySession("ses-backup")
	if err != nil {
		t.Fatalf("read restored messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "the only copy of this message" {
		t.Fatalf("restored messages = %+v, want the single seeded message", msgs)
	}
}

// TestBackupFilesAreOwnerOnlyLos38y: a backup holds full transcripts, and its
// directory is a command argument that can be anywhere, so the files must not keep
// the umask mode VACUUM INTO creates them with.
func TestBackupFilesAreOwnerOnlyLos38y(t *testing.T) {
	_, backupDir := los38yInstall(t)

	los38yRunBackup(t, backupDir)

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("backup dir %s is empty", backupDir)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", e.Name(), got)
		}
	}
}

// TestBackupDirIsOwnerOnlyLos38y: MkdirAll leaves the mode of a directory it did not
// create, so an existing 0755 backup directory has to be closed before the snapshot
// is written into it.
func TestBackupDirIsOwnerOnlyLos38y(t *testing.T) {
	_, backupDir := los38yInstall(t)

	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("create backup dir: %v", err)
	}
	if err := os.Chmod(backupDir, 0o755); err != nil {
		t.Fatalf("loosen backup dir: %v", err)
	}

	los38yRunBackup(t, backupDir)

	info, err := os.Stat(backupDir)
	if err != nil {
		t.Fatalf("stat backup dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("backup dir mode = %04o, want 0700", got)
	}
}

func los38yReplaceFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.Remove(dst); err != nil {
		t.Fatalf("remove %s: %v", dst, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

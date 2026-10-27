package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// YM-268 [CFG-03] (Los 55c), Befund 4: setup schrieb config.yaml und die
// Sicherung mit 0644. Die Datei traegt api.api_key und api.openai_api_key, also
// war sie fuer jeden lokalen Nutzer lesbar; der Rest des Setups haertet das
// Datenverzeichnis dagegen auf 0700.
func TestLos55c_SetupWritesConfigOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatalf("seed config.yaml: %v", err)
	}

	if err := backupAndWriteConfigFile(dir, "proxy:\n  enabled: false\n"); err != nil {
		t.Fatalf("write config: %v", err)
	}

	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat config.yaml: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.yaml mode = %04o, want 0600 (the file carries the API keys)", perm)
	}

	backups, err := filepath.Glob(filepath.Join(dir, "backup", "config.yaml.bak.*"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	info, err = os.Stat(backups[0])
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %04o, want 0600 (a copy of config.yaml carries the API keys)", perm)
	}
}

// Cold review finding 4: setup's own backups live in <dataDir>/backup, where the
// runtime prune (which scans the directory of config.yaml) never looks. Repeated
// setup or update runs therefore left an unbounded pile of API-key copies.
func TestLos55c_SetupBackupsStayBounded(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(dir, "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("mkdir backup dir: %v", err)
	}
	for i := 0; i < 12; i++ {
		name := filepath.Join(backupDir, fmt.Sprintf("config.yaml.bak.20250101T0000%02d", i))
		if err := os.WriteFile(name, []byte("api:\n  api_key: old\n"), 0o600); err != nil {
			t.Fatalf("seed backup %d: %v", i, err)
		}
	}

	if err := backupAndWriteConfigFile(dir, "proxy:\n  enabled: true\n"); err != nil {
		t.Fatalf("write config: %v", err)
	}

	backups, err := filepath.Glob(filepath.Join(backupDir, "config.yaml.bak.*"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) > 10 {
		t.Errorf("%d setup backups retained, want at most 10 (each carries the API keys)", len(backups))
	}
}

// Cold review 5.4: the backup directory lives inside the 0700 data directory and
// holds copies of the key file. It was created 0755.
func TestLos55c_SetupBackupDirIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("proxy:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("seed config.yaml: %v", err)
	}
	if err := backupAndWriteConfigFile(dir, "proxy:\n  enabled: false\n"); err != nil {
		t.Fatalf("write config: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "backup"))
	if err != nil {
		t.Fatalf("stat backup dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("backup dir mode = %04o, want 0700 (it holds copies of the key file)", perm)
	}
}

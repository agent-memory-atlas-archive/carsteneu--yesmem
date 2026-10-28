package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-392 [CFG-04] Teil 5 (Los 56i): setup sichert die vorhandene config.yaml
// unter einem Namen mit Sekundenaufloesung. Zwei Laeufe in derselben Sekunde
// treffen denselben Namen, und die zweite Sicherung ueberschreibt die erste.
// Damit ist der Stand vor dem ersten Lauf verloren, obwohl zwei Kopien
// erwartet werden. config.backupFile loest das seit 55c mit einem Zaehler-
// Suffix; der setup-Pfad zog nicht mit.
func TestLos56i_SetupConfigBackupsDoNotCollideWithinASecond(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := filepath.Join(dataDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n# first\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := backupAndWriteConfigFile(dataDir, "proxy:\n  enabled: true\n# second\n"); err != nil {
		t.Fatalf("first setup write: %v", err)
	}
	if err := backupAndWriteConfigFile(dataDir, "proxy:\n  enabled: true\n# third\n"); err != nil {
		t.Fatalf("second setup write: %v", err)
	}

	backupDir := filepath.Join(dataDir, "backup")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "config.yaml.bak.") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("got %d config backups (%v), want 2: the second run overwrote the first", len(names), names)
	}

	// Both states must still be readable: the oldest copy carries "# first",
	// the newer one "# second". A collision leaves only the newer state twice.
	var contents []string
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(backupDir, name))
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, string(data))
	}
	joined := strings.Join(contents, "\n---\n")
	if !strings.Contains(joined, "# first") || !strings.Contains(joined, "# second") {
		t.Errorf("the two backups do not hold the two distinct pre-run states:\n%s", joined)
	}
}

// YM-392 Teil 5, zweiter Halbsatz: ein bereits vorhandenes Backup-Verzeichnis
// mit 0755 wird durch MkdirAll(0700) nicht nachtraeglich gehaertet. Die Kopien
// tragen die API-Keys, also muss das Verzeichnis beim Schreiben owner-only
// werden.
func TestLos56i_SetupConfigBackupDirIsHardenedOnWrite(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := filepath.Join(dataDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dataDir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backupDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := backupAndWriteConfigFile(dataDir, "proxy:\n  enabled: false\n"); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	info, err := os.Stat(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("backup dir mode = %04o, want 0700: the directory holds copies of the API keys", perm)
	}
}

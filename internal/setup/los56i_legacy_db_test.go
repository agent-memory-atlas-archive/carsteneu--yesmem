package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-180 [LIVE-5] (Los 56i): im Datenverzeichnis liegen Datenbankdateien, die
// kein Code mehr oeffnet: capabilities.db (Vorgaenger der Cap-Ablage, heute
// caps.db), cap_store.db und die Datei mit dem woertlichen Namen
// "capabilities*.db" (Reste einer nicht expandierten Shell-Glob). Sie sammeln
// sich an, werden nie gelesen und tauchen in jedem Audit wieder auf.
func TestLos56i_RemoveLegacyDBFilesDeletesOnlyTheOrphans(t *testing.T) {
	dataDir := t.TempDir()
	orphans := []string{"capabilities.db", "cap_store.db", "capabilities*.db"}
	for _, name := range orphans {
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A live database and its siblings must survive.
	live := []string{"yesmem.db", "yesmem.db-wal", "runtime.db", "caps.db", "messages.db"}
	for _, name := range live {
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	removed := RemoveLegacyDBFiles(dataDir)

	if len(removed) != len(orphans) {
		t.Errorf("removed %v, want all of %v", removed, orphans)
	}
	for _, name := range orphans {
		if _, err := os.Stat(filepath.Join(dataDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived (err=%v)", name, err)
		}
	}
	for _, name := range live {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err != nil {
			t.Errorf("live database file %s was removed: %v", name, err)
		}
	}

	// Moved, not deleted: a v1 cap store can hold scripts that exist nowhere else.
	backups, err := filepath.Glob(filepath.Join(dataDir, "backup", "legacy-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("legacy files were not moved into a backup directory (glob=%v err=%v)", backups, err)
	}
	for _, name := range orphans {
		if _, err := os.Stat(filepath.Join(backups[0], name)); err != nil {
			t.Errorf("%s was not preserved in the backup: %v", name, err)
		}
	}
}

// Ein zweiter Lauf ist folgenlos: die Migration muss beliebig oft laufen.
func TestLos56i_RemoveLegacyDBFilesIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "capabilities.db"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	if removed := RemoveLegacyDBFiles(dataDir); len(removed) != 1 {
		t.Fatalf("first run removed %v, want one file", removed)
	}
	if removed := RemoveLegacyDBFiles(dataDir); len(removed) != 0 {
		t.Errorf("second run removed %v, want nothing", removed)
	}
}

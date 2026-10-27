package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-350 [PLG-F30] Teil 2 (Los 56i): die Selbstloeschung entfernt das Binary,
// auf das os.Executable zeigt (auf Linux /proc/self/exe, also den aufgeloesten
// Pfad). Ein Link, ueber den der Nutzer yesmem tatsaechlich aufruft, liegt
// danach als haengender Eintrag im PATH.
func TestLos56i_RemoveStaleBinaryLinksRemovesOnlyLinksToTheRemovedBinary(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "yesmem")
	if err := os.WriteFile(target, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "yesmem")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	// Ein fremder Link und eine regulaere Datei im selben Verzeichnis bleiben.
	otherTarget := filepath.Join(t.TempDir(), "other-tool")
	if err := os.WriteFile(otherTarget, []byte("other"), 0755); err != nil {
		t.Fatal(err)
	}
	otherLink := filepath.Join(binDir, "other-tool")
	if err := os.Symlink(otherTarget, otherLink); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(binDir, "third-tool")
	if err := os.WriteFile(regular, []byte("third"), 0755); err != nil {
		t.Fatal(err)
	}

	removeStaleBinaryLinks(home, target)

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the link to the removed binary survived (err=%v)", err)
	}
	if _, err := os.Lstat(otherLink); err != nil {
		t.Errorf("a foreign link was removed: %v", err)
	}
	if _, err := os.Stat(regular); err != nil {
		t.Errorf("a regular file was removed: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the link target was removed: %v", err)
	}
}

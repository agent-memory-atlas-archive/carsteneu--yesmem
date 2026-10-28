package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-386 [PLG-F34] Teil 2 (Los 56i): der Install-Pfad von
// InstallOpencodeCommands schrieb ohne die beiden Schutzregeln, die der
// Uninstall-Sweep seit 18j hat. Ein Symlink auf das Zielverzeichnis traegt die
// Writes nach draussen, ein Symlink an einer Command-Datei laesst os.WriteFile
// in das Linkziel schreiben.
func TestLos56i_InstallOpencodeCommandsRefusesASymlinkedDirectory(t *testing.T) {
	home := t.TempDir()
	victim := t.TempDir()
	dir := filepath.Join(home, opencodeCommandsRelDir)
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallOpencodeCommands(home); err == nil {
		t.Fatal("the installer wrote into a symlinked commands directory")
	}

	entries, err := os.ReadDir(victim)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the installer wrote %d file(s) through the symlink into %s", len(entries), victim)
	}
}

// Ein Symlink an der Zieldatei ist derselbe Ausbruch eine Ebene tiefer: die
// Datei existiert fuer ReadFile, aber der Write landet im Linkziel.
func TestLos56i_InstallOpencodeCommandsDoesNotWriteThroughASymlinkedFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, opencodeCommandsRelDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	names, err := bundledOpencodeCommandNames()
	if err != nil || len(names) == 0 {
		t.Fatalf("bundle names: %v (%d)", err, len(names))
	}

	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "victim.md")
	const original = "operator content\n"
	if err := os.WriteFile(victim, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, names[0])
	if err := os.Symlink(victim, dst); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallOpencodeCommands(home); err == nil {
		t.Fatal("the installer overwrote a symlinked command file")
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("the installer wrote through the symlink: victim now holds %q", string(got))
	}
}

// Ein Name aus dem Bundle, der kein reiner Eintragsname ist, darf nie als Pfad
// unter dem Zielverzeichnis landen. Der Bundle-Inhalt ist eingebettet, also
// prueft der Test die Filterschicht, die der Installer benutzt.
func TestLos56i_BundledOpencodeCommandNamesArePlain(t *testing.T) {
	names, err := bundledOpencodeCommandNames()
	if err != nil {
		t.Fatalf("bundle names: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("the bundle names no opencode command")
	}
	for _, name := range names {
		if !isPlainEntryName(name) {
			t.Errorf("bundle entry %q is not a plain name but the installer would write %q", name, filepath.Join("commands", name))
		}
	}
}

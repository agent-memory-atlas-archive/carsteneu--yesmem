package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-181/YM-197, Cold-Review-Befund 1 (Los 56i): der 30-Sekunden-CAP.md-Watcher
// ist ein dritter Importpfad und ging an der Ausmusterung vorbei. ScanChanged
// startet mit leerer mtime-Map, also gilt nach jedem Daemon-Start jede CAP.md
// als geaendert; handleSaveCap sucht den Vorgaenger nur unter den AKTIVEN Caps,
// die ausgemusterte Zeile ist das nicht, und legt deshalb eine neue aktive Zeile
// an. Ergebnis: wiki_export ist 30 Sekunden nach dem naechsten Start wieder im
// Katalog.
func TestLos56i_CapWatcherCannotResurrectARetiredCap(t *testing.T) {
	h, store := mustHandler(t)
	dir := t.TempDir()

	los32bWriteDiskCapFile(t, dir, "wiki_export", 3, "echo retired\n")
	los32bWriteDiskCapFile(t, dir, "still_shipped", 3, "echo live\n")

	watcher := NewCapsDirWatcher()
	// The ticker's body, as daemon.go runs it.
	changed := watcher.ScanChanged(dir)
	for _, cf := range changed {
		h.handleSaveCap(CapFileToParams(cf))
		watcher.RefreshMtime(cf.SourcePath)
	}

	if meta := storedCapMeta(t, store, "wiki_export"); meta != nil {
		t.Errorf("the watcher imported the retired cap again (active row %q)", meta.Name)
	}
	if meta := storedCapMeta(t, store, "still_shipped"); meta == nil {
		t.Error("the watcher no longer imports a cap that is still shipped")
	}

	// Even after a retirement the file must not come back through this path.
	los32bWriteDiskCapFile(t, dir, "wiki_export", 4, "echo retired again\n")
	for _, cf := range watcher.ScanChanged(dir) {
		h.handleSaveCap(CapFileToParams(cf))
		watcher.RefreshMtime(cf.SourcePath)
	}
	if meta := storedCapMeta(t, store, "wiki_export"); meta != nil {
		t.Errorf("a changed retired CAP.md was imported again (active row %q)", meta.Name)
	}
}

// Die Ausmusterung als Regel, nicht als Einmal-Migration: save_cap ist der
// Sammelpunkt aller Importpfade und muss den Namen selbst ablehnen, sonst legt
// ein Agent, der einer Recipe folgt, das Cap sofort wieder an.
func TestLos56i_SaveCapRefusesARetiredName(t *testing.T) {
	h, store := mustHandler(t)

	resp := h.handleSaveCap(map[string]any{
		"name":        "wiki_export",
		"description": "recreated by hand",
		"scope":       "user",
		"scripts":     `[{"name":"run","kind":"tool","runtime":"bash","body":"echo hi","schema":"{}"}]`,
	})
	if resp.Error == "" {
		t.Fatal("save_cap recreated a retired cap")
	}
	if meta := storedCapMeta(t, store, "wiki_export"); meta != nil {
		t.Errorf("a retired cap row was stored: %q", meta.Name)
	}

	// The file must not be written back either: WriteCapToDisk runs after the save.
	if _, err := os.Stat(filepath.Join(h.dataDir, "caps")); err == nil {
		// Only meaningful when the save path was reached at all.
		if _, err := os.Stat(filepath.Join(h.dataDir, "caps", "wiki_export", "CAP.md")); err == nil {
			t.Error("a retired CAP.md was written to disk")
		}
	}
}

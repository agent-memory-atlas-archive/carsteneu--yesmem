package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/capfile"
)

// The watcher skips a path whose mtime is not newer than the stored one. A
// rejected read (a planted link, a file with two hard links, a short EACCES) must
// therefore not store the mtime: the repaired file commonly keeps the timestamp of
// the backup it was restored from, and the path would stay unimported for the rest
// of the daemon's life. Reported by the cold review of this loss (YM-373 key 2).
func TestLos56gCapsDirWatcherImportsARepairedFileWithAnOlderMtime(t *testing.T) {
	elsewhere := filepath.Join(t.TempDir(), "los56g_repaired.md")
	cf := &capfile.CapFile{
		Name: "los56g_repaired", Description: "repaired", Version: 1, Scope: "user", Purpose: "Test.",
		Scripts: []capfile.Script{{Name: "run", Kind: "handler", Runtime: "bash", Lang: "bash", Body: "echo repaired"}},
	}
	if err := capfile.WriteOwnerOnly(elsewhere, capfile.Render(cf)); err != nil {
		t.Fatalf("write the link target: %v", err)
	}
	watched := t.TempDir()
	capDir := filepath.Join(watched, "los56g_repaired")
	if err := os.MkdirAll(capDir, 0o700); err != nil {
		t.Fatalf("mkdir cap dir: %v", err)
	}
	capPath := filepath.Join(capDir, "CAP.md")
	if err := os.Symlink(elsewhere, capPath); err != nil {
		t.Fatalf("plant the link: %v", err)
	}

	w := NewCapsDirWatcher()
	if got := w.ScanChanged(watched); len(got) != 0 {
		t.Fatalf("the planted link was imported: %d cap(s)", len(got))
	}

	// The repair: the operator replaces the link with the real file, keeping the
	// timestamp of the backup it came from.
	if err := os.Remove(capPath); err != nil {
		t.Fatalf("remove the link: %v", err)
	}
	if err := os.WriteFile(capPath, capfile.Render(cf), 0o600); err != nil {
		t.Fatalf("write the repaired file: %v", err)
	}
	older := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(capPath, older, older); err != nil {
		t.Fatalf("set the mtime: %v", err)
	}

	got := w.ScanChanged(watched)

	if len(got) != 1 || got[0].Name != "los56g_repaired" {
		t.Fatalf("the repaired file was not imported (%d cap(s)): a rejected read must not leave a mtime behind", len(got))
	}
}

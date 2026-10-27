package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/capfile"
)

// The caps directory watcher is the second import path of YM-373 key 2: it read
// CAP.md with os.Stat plus os.ReadFile, so a link planted in the watched
// directory was imported as a cap.
func TestLos56gCapsDirWatcherRefusesASymlinkedCapFile(t *testing.T) {
	elsewhere := filepath.Join(t.TempDir(), "los56g_planted.md")
	cf := &capfile.CapFile{
		Name: "los56g_planted", Description: "planted", Version: 1, Scope: "user", Purpose: "Test.",
		Scripts: []capfile.Script{{Name: "run", Kind: "handler", Runtime: "bash", Lang: "bash", Body: "echo planted"}},
	}
	if err := capfile.WriteOwnerOnly(elsewhere, capfile.Render(cf)); err != nil {
		t.Fatalf("write the link target: %v", err)
	}
	watched := t.TempDir()
	capDir := filepath.Join(watched, "los56g_planted")
	if err := os.MkdirAll(capDir, 0o700); err != nil {
		t.Fatalf("mkdir cap dir: %v", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(capDir, "CAP.md")); err != nil {
		t.Fatalf("plant the link: %v", err)
	}

	got := NewCapsDirWatcher().ScanChanged(watched)

	if len(got) != 0 {
		t.Errorf("the watcher imported %d cap(s) through a symlink, want 0", len(got))
	}
}

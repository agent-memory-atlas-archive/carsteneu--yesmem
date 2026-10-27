package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/capfile"
)

// YM-359 (OPS-88, key 3): the export branch repaired the mode of an existing
// CAP.md with os.Chmod, which follows a symlink. A link planted at that path
// handed the mode change to its target, and the version gate above it read the
// link's content as if it were the cap.
func TestLos35fWriteCapToDiskDoesNotChmodThroughASymlink(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	meta := CapMeta{
		Name:        "los35f_symlink_cap",
		Description: "test cap",
		Scripts:     []ScriptMeta{{Name: "run", Kind: "handler", Runtime: "bash", Body: "echo hi"}},
		Version:     1,
		Tested:      true,
		Scope:       "user",
	}
	seedCapMeta(t, h.store, meta, "")

	victim := filepath.Join(t.TempDir(), "victim")
	cf := CapMetaToCapFile(meta, "", ProfileStandard)
	if err := os.WriteFile(victim, capfile.Render(&cf), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	baseDir := t.TempDir()
	capDir := filepath.Join(baseDir, meta.Name)
	if err := os.MkdirAll(capDir, 0o700); err != nil {
		t.Fatalf("mkdir cap dir: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(capDir, "CAP.md")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	err := WriteCapToDisk(meta, "", baseDir, h.store, ProfileStandard)
	if err == nil {
		t.Fatal("WriteCapToDisk accepted a symlinked CAP.md")
	}

	info, statErr := os.Stat(victim)
	if statErr != nil {
		t.Fatalf("stat victim: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("the mode repair followed the symlink, victim mode = %04o, want 0644", perm)
	}
}

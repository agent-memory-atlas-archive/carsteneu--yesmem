package capfile

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-353: os.Chmod follows a symlink, so the directory repair must not be
// applied to a path whose final component is a link. A symlink planted at a cap
// path would otherwise hand the mode change to its target (`/etc` in the worst
// case). The write is refused instead.
func TestLos35eWriteFileRefusesASymlinkedDir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatalf("chmod target: %v", err)
	}
	link := filepath.Join(root, "cap_dir")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := WriteFile(los35eProbeCap(), link); err == nil {
		t.Fatal("WriteFile followed a symlinked cap dir")
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("the symlink target mode changed to %04o, want 0755 untouched", perm)
	}
	if _, err := os.Stat(filepath.Join(target, "CAP.md")); err == nil {
		t.Error("the cap landed in the symlink target")
	}
}

package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-359 (OPS-88, key 3): InstallBundledCaps wrote every bundled CAP.md with
// os.WriteFile and then repaired its mode with os.Chmod. Both follow a symlink,
// so a link planted in the caps directory received the bundled body and the mode
// change on whatever file it named.
func TestLos35fInstallBundledCapsDoesNotWriteThroughASymlink(t *testing.T) {
	capsDir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	capDir := filepath.Join(capsDir, "telegram")
	if err := os.MkdirAll(capDir, 0o700); err != nil {
		t.Fatalf("mkdir cap dir: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(capDir, "CAP.md")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if _, _, err := InstallBundledCaps(capsDir); err != nil {
		t.Fatalf("InstallBundledCaps: %v", err)
	}

	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(data) != "untouched" {
		t.Fatalf("a bundled cap body was written through the symlink, victim = %q", data)
	}
	info, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("stat victim: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("the mode repair followed the symlink, victim mode = %04o, want 0644", perm)
	}
}

package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/caps"
)

// YM-346/OPS-85, key 4b: capfile.WriteFile learned to write owner-only, but the
// bundle installer is the path that puts the bundled caps on disk — and it wrote
// 0644. It is also the only writer that runs on a fresh install, so it would have
// undone the hardening for exactly the eleven files this loss touches.
func TestLos35cInstallBundledCapsWritesOwnerOnly(t *testing.T) {
	home := t.TempDir()
	if _, _, err := InstallBundledCaps(filepath.Join(home, ".claude", "caps")); err != nil {
		t.Fatalf("install bundled caps: %v", err)
	}

	path := filepath.Join(home, ".claude", "caps", "telegram", "CAP.md")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the bundled telegram cap was not installed: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("installed CAP.md mode = %04o, want 0600", perm)
	}
}

// os.WriteFile only applies its mode when it creates the file, so a CAP.md that
// was installed before this fix keeps its 0644 forever without an explicit chmod
// — the hardening would reach new installs only.
func TestLos35cInstallBundledCapsRepairsAnExistingMode(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "caps", "telegram", "CAP.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Different content, so the installer does not skip the file as unchanged.
	if err := os.WriteFile(path, []byte("stale copy\n"), 0o600); err != nil {
		t.Fatalf("write stale cap: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod stale cap: %v", err)
	}

	if _, _, err := InstallBundledCaps(filepath.Join(home, ".claude", "caps")); err != nil {
		t.Fatalf("install bundled caps: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode after the update = %04o, want 0600: an existing file keeps the mode it was created with", perm)
	}
}

// The content comparison short-circuits before the write, so a file whose content
// is already current never reaches the write path — its mode has to be repaired on
// that branch too, or the 0644 files this loss exists for would stay that way.
func TestLos35cInstallBundledCapsRepairsModeWhenContentMatches(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "caps", "telegram", "CAP.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bundled, err := caps.BundledCaps.ReadFile("bundled-caps/telegram/CAP.md")
	if err != nil {
		t.Fatalf("read bundled cap: %v", err)
	}
	if err := os.WriteFile(path, bundled, 0o600); err != nil {
		t.Fatalf("write current copy: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod current copy: %v", err)
	}

	if _, _, err := InstallBundledCaps(filepath.Join(home, ".claude", "caps")); err != nil {
		t.Fatalf("install bundled caps: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode of an unchanged file = %04o, want 0600", perm)
	}
}

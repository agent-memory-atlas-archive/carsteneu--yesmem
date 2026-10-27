package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-353 (OPS-87, key 3): the installer derived the caps dir from the home
// directory, the daemon from the data dir. With the data dir outside ~/.claude the
// bundled caps landed in ~/.claude/caps and nothing imported them.
func TestLos35eCapsDirForSetupFollowsTheDataDir(t *testing.T) {
	home := t.TempDir()

	for _, tt := range []struct{ dataDir, want string }{
		{filepath.Join(home, ".claude", "yesmem"), filepath.Join(home, ".claude", "caps")},
		{filepath.Join(home, "xdg-data", "yesmem"), filepath.Join(home, "xdg-data", "caps")},
	} {
		if got := los35eCapsDirForSetup(tt.dataDir); got != tt.want {
			t.Errorf("caps dir for data dir %q = %q, want %q", tt.dataDir, got, tt.want)
		}
	}
}

// The same invariant for the installer: it is the only writer that runs on a fresh
// install, so it would have undone the hardening for every bundled cap. The root
// directory counts too — an install created before YM-353 has it 0755, and its
// listing alone names every installed cap.
func TestLos35eBundledCapsLandOwnerOnly(t *testing.T) {
	home := t.TempDir()
	capsDir := filepath.Join(home, "caps")
	if err := os.MkdirAll(capsDir, 0o755); err != nil {
		t.Fatalf("mkdir caps dir: %v", err)
	}
	if err := os.Chmod(capsDir, 0o755); err != nil {
		t.Fatalf("chmod caps dir: %v", err)
	}

	if _, _, err := InstallBundledCaps(capsDir); err != nil {
		t.Fatalf("install bundled caps: %v", err)
	}

	for _, dir := range []string{capsDir, filepath.Join(capsDir, "telegram")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %04o, want 0700", dir, perm)
		}
	}
}

// A relative caps dir is installed under the caller's working directory, where
// nothing imports it; the export path refuses the same case.
func TestLos35eInstallBundledCapsRejectsARelativeDir(t *testing.T) {
	if _, _, err := InstallBundledCaps("caps"); err == nil {
		t.Fatal("InstallBundledCaps accepted a relative caps dir")
	}
}

// The end the resolver exists for: an install whose data dir sits outside
// ~/.claude has to receive the bundled caps where the daemon reads them.
func TestLos35eBundledCapsLandWhereTheDaemonImports(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, "xdg-data", "yesmem")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	capsDir := los35eCapsDirForSetup(dataDir)
	if _, _, err := InstallBundledCaps(capsDir); err != nil {
		t.Fatalf("install bundled caps: %v", err)
	}

	installed := filepath.Join(capsDir, "telegram", "CAP.md")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("the bundled cap did not land in the directory the daemon imports: %v", err)
	}
	// Two locations would diverge again on the next export.
	if _, err := os.Stat(filepath.Join(home, ".claude", "caps", "telegram", "CAP.md")); err == nil {
		t.Errorf("the installer also wrote the home-derived caps dir")
	}
}

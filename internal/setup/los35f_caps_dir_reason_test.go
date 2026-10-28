package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// YM-359 (OPS-88, key 4): the YM-353 commit justified the shared derivation with
// a data dir outside ~/.claude. The installer never sees one — Run pins dataDir to
// ~/.claude/yesmem — so that sentence claimed a case it cannot reach. What this
// path does need is an assertion: its target equals the directory the daemon
// imports from for the config the same run writes.
func TestLos35fSetupCapsDirEqualsTheDaemonImportPath(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")

	got := los35eCapsDirForSetup(dataDir)
	want := filepath.Join(home, ".claude", "caps")

	if got != want {
		t.Fatalf("installer target %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("installer target is not absolute: %q", got)
	}
	if daemon := config.CapsDirPath("", dataDir); daemon != want {
		t.Fatalf("daemon import path %q, installer target %q", daemon, got)
	}
}

// The stale configured value in the file this run is about to overwrite must not
// move the install. Reading it here would install the bundled caps into a
// directory the freshly written config no longer names.
func TestLos35fSetupCapsDirIgnoresAStaleConfiguredValue(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	cfgPath := filepath.Join(dataDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("caps_dir: /somewhere/else\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got := los35eCapsDirForSetup(dataDir)
	if want := filepath.Join(home, ".claude", "caps"); got != want {
		t.Fatalf("a stale configured caps_dir moved the install: got %q, want %q", got, want)
	}
}

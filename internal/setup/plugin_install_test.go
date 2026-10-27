package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/buildinfo"
)

// --- install hygiene (YM-86) ---
//
// The installer mirrors the embedded bundle and never deleted anything, so the
// installed plugin directory kept files the repo had removed and shipped the
// plugin's own test files. The directory is generated output: it mirrors the
// bundle. Exercised on the v2 bundle, the only plugin yesmem ships.

func TestInstallOpencodePluginV2_InstallsBundle(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")

	if _, err := installOpencodePluginV2(home); err != nil {
		t.Fatalf("installOpencodePluginV2: %v", err)
	}

	dstDir := v2InstallDir(home)
	data, err := os.ReadFile(filepath.Join(dstDir, "index.ts"))
	if err != nil {
		t.Fatalf("index.ts not installed: %v", err)
	}
	if len(data) == 0 {
		t.Error("index.ts is empty")
	}
	for _, name := range []string{"rpc.ts", "types.ts", "rule_guard.ts"} {
		if _, err := os.Stat(filepath.Join(dstDir, name)); os.IsNotExist(err) {
			t.Errorf("%s not installed", name)
		}
	}
}

func TestInstallOpencodePluginV2_Idempotent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")

	if _, err := installOpencodePluginV2(home); err != nil {
		t.Fatal(err)
	}
	if _, err := installOpencodePluginV2(home); err != nil {
		t.Fatalf("second installOpencodePluginV2: %v", err)
	}
}

func TestInstallOpencodePluginV2_DoesNotInstallTestFiles(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if _, err := installOpencodePluginV2(home); err != nil {
		t.Fatalf("installOpencodePluginV2: %v", err)
	}

	dstDir := v2InstallDir(home)
	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatalf("read installed dir: %v", err)
	}
	for _, e := range entries {
		// Two naming conventions meet here: the plugin's own long-standing `_test.ts` and the
		// `.test.ts` the suite inherited from the opencode fork. The fixture is the suite's shared
		// harness, not a plugin source either.
		if strings.HasSuffix(e.Name(), "_test.ts") || strings.HasSuffix(e.Name(), ".test.ts") {
			t.Errorf("test file installed: %s", e.Name())
		}
		if e.Name() == "opencode-yesmem-fixture.ts" {
			t.Errorf("test harness installed: %s", e.Name())
		}
	}
	for _, name := range []string{"index.ts", "rpc.ts", "rule_guard.ts"} {
		if _, err := os.Stat(filepath.Join(dstDir, name)); err != nil {
			t.Errorf("%s missing after install: %v", name, err)
		}
	}
}

func TestInstallOpencodePluginV2_RemovesStaleFiles(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	dstDir := v2InstallDir(home)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Files a previous yesmem version installed and the repo has since dropped.
	for _, name := range []string{"old_hook.ts", "messages_test.ts"} {
		if err := os.WriteFile(filepath.Join(dstDir, name), []byte("// old"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := installOpencodePluginV2(home); err != nil {
		t.Fatalf("installOpencodePluginV2: %v", err)
	}

	for _, name := range []string{"old_hook.ts", "messages_test.ts"} {
		if _, err := os.Stat(filepath.Join(dstDir, name)); !os.IsNotExist(err) {
			t.Errorf("stale file %s survived the install (stat err=%v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dstDir, "index.ts")); err != nil {
		t.Errorf("index.ts missing after install: %v", err)
	}
}

func TestInstallOpencodePluginV2_ReportsUpdatedCount(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	n, err := installOpencodePluginV2(home)
	if err != nil {
		t.Fatalf("installOpencodePluginV2: %v", err)
	}
	if n == 0 {
		t.Error("first install reported 0 updated files")
	}
	if n, err = installOpencodePluginV2(home); err != nil {
		t.Fatalf("second installOpencodePluginV2: %v", err)
	}
	if n != 0 {
		t.Errorf("idempotent install reported %d updated files, want 0", n)
	}
}

// --- plugin manifest (YM-421) ---
//
// opencode reports a directory plugin's version from its package.json; without one the listing says
// "local" and two installs of different yesmem versions are indistinguishable. The manifest is
// generated, not embedded, because the version has to be the one of the installing binary.

func readInstalledManifest(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(v2InstallDir(home), "package.json"))
	if err != nil {
		t.Fatalf("package.json not installed: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("installed package.json is not valid JSON: %v", err)
	}
	return manifest
}

func TestInstallOpencodePluginV2_WritesManifest(t *testing.T) {
	previous := buildinfo.Version
	buildinfo.Version = "9.9.9-los58f"
	defer func() { buildinfo.Version = previous }()

	home := filepath.Join(t.TempDir(), "home")
	if _, err := installOpencodePluginV2(home); err != nil {
		t.Fatalf("installOpencodePluginV2: %v", err)
	}

	manifest := readInstalledManifest(t, home)
	if manifest["name"] != "opencode-yesmem" {
		t.Errorf("name = %v, want opencode-yesmem", manifest["name"])
	}
	if manifest["version"] != "9.9.9-los58f" {
		t.Errorf("version = %v, want the version of the installing binary", manifest["version"])
	}
	if manifest["type"] != "module" {
		t.Errorf("type = %v, want module", manifest["type"])
	}
	// The repo's own package.json is the suite's dev manifest and carries effect. Shipping it would
	// ask a plugin host to install a build dependency into the user's config.
	if dev, ok := manifest["devDependencies"]; ok {
		t.Errorf("installed manifest carries devDependencies: %v", dev)
	}
}

func TestInstallOpencodePluginV2_ManifestFollowsTheVersion(t *testing.T) {
	previous := buildinfo.Version
	defer func() { buildinfo.Version = previous }()

	home := filepath.Join(t.TempDir(), "home")
	buildinfo.Version = "1.0.0"
	if _, err := installOpencodePluginV2(home); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if got := readInstalledManifest(t, home)["version"]; got != "1.0.0" {
		t.Fatalf("version after first install = %v, want 1.0.0", got)
	}

	buildinfo.Version = "1.0.1"
	n, err := installOpencodePluginV2(home)
	if err != nil {
		t.Fatalf("install after version bump: %v", err)
	}
	if got := readInstalledManifest(t, home)["version"]; got != "1.0.1" {
		t.Errorf("version after bump = %v, want 1.0.1", got)
	}
	if n != 1 {
		t.Errorf("version bump reported %d updated files, want 1 (the manifest)", n)
	}
}

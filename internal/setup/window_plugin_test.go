package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// windowHome prepares a fake HOME with an existing ~/.config/opencode dir
// (the opencode integration is active) and an optional pre-installed plugin.
func windowHome(t *testing.T, installed string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0755); err != nil {
		t.Fatal(err)
	}
	if installed != "" {
		dst := filepath.Join(home, ".config", "opencode", "plugins", "yesmem-window", "tui.tsx")
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(installed), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestInstallWindowPlugin_InstallsFile(t *testing.T) {
	home := windowHome(t, "")

	n, err := InstallWindowPlugin(home)
	if err != nil {
		t.Fatalf("InstallWindowPlugin: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated = %d, want 1", n)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "plugins", "yesmem-window", "tui.tsx"))
	if err != nil {
		t.Fatalf("plugin file missing: %v", err)
	}
	if !strings.Contains(string(data), "yesmem.window-register") {
		t.Error("installed file does not look like the window plugin")
	}
}

func TestInstallWindowPlugin_IdempotentAndUpdating(t *testing.T) {
	home := windowHome(t, "stale content\n")

	if n, err := InstallWindowPlugin(home); err != nil || n != 1 {
		t.Fatalf("first run = (%d, %v), want (1, nil)", n, err)
	}
	if n, err := InstallWindowPlugin(home); err != nil || n != 0 {
		t.Fatalf("second run = (%d, %v), want (0, nil)", n, err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "plugins", "yesmem-window", "tui.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "stale content") {
		t.Error("stale plugin was not replaced")
	}
}

// Claude-only users have no ~/.config/opencode: no plugin dir may be created
// behind their back.
func TestInstallWindowPlugin_SkipsWithoutOpencodeConfig(t *testing.T) {
	home := t.TempDir()

	n, err := InstallWindowPlugin(home)
	if err != nil {
		t.Fatalf("InstallWindowPlugin: %v", err)
	}
	if n != 0 {
		t.Fatalf("updated = %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode")); !os.IsNotExist(err) {
		t.Error("~/.config/opencode was created for a user without opencode")
	}
}

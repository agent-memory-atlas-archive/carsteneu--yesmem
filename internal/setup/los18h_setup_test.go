package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The uninstall announcement and the deletion have to name the same opencode
// artifacts. Los 18f found them apart: the text promised one file, removeOpencodePlugin
// deleted three entries and the theme family as well. The expectation here is
// written out independently of the implementation, so it cannot follow a wrong
// list silently.
func TestUninstallPreviewNamesEveryOpencodeArtifactItRemoves(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	pluginsDir := filepath.Join(home, ".config", "opencode", "plugins")
	for _, name := range []string{"yesmem.ts", "opencode-yesmem", "yesmem-window"} {
		if err := os.MkdirAll(filepath.Join(pluginsDir, name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginsDir, name, "index.ts"), []byte("// plugin"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	themesDir := filepath.Join(home, ".config", "opencode", ThemeDir)
	if err := os.MkdirAll(themesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(themesDir, "yesmem-default.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// A leftover v1 install: removeOpencodePlugin deletes its source tree once
	// the config is rewritable, so the announcement has to name it too.
	v1SourceDir := filepath.Join(home, filepath.Dir(v1PluginPersistentRelPath))
	if err := os.MkdirAll(v1SourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1SourceDir, "index.ts"), []byte("// v1"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := removeOpencodePlugin(home); err != nil {
		t.Fatalf("removeOpencodePlugin: %v", err)
	}

	preview := strings.Join(uninstallPreviewLines(), "\n")
	for _, name := range []string{"yesmem.ts", "opencode-yesmem", "yesmem-window"} {
		if _, err := os.Lstat(filepath.Join(pluginsDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the uninstall: lstat err=%v", name, err)
		}
		if !strings.Contains(preview, name) {
			t.Errorf("the uninstall announcement does not name the removed plugin entry %s:\n%s", name, preview)
		}
	}
	if _, err := os.Stat(filepath.Join(themesDir, "yesmem-default.json")); !os.IsNotExist(err) {
		t.Errorf("the installed theme survived the uninstall: %v", err)
	}
	if !strings.Contains(preview, "opencode/"+ThemeDir) {
		t.Errorf("the uninstall announcement does not name the removed theme directory:\n%s", preview)
	}
	if _, err := os.Stat(v1SourceDir); !os.IsNotExist(err) {
		t.Errorf("the v1 plugin source tree survived the uninstall: %v", err)
	}
	if !strings.Contains(preview, "share/yesmem/plugins/opencode-yesmem") {
		t.Errorf("the uninstall announcement does not name the removed v1 plugin source tree:\n%s", preview)
	}
}

// The built-in probe reads one binary: the one setup would start, found on PATH.
// opencode keeps one plugin directory per home, so the answer cannot speak for a
// build started from somewhere else. The status line therefore has to name the
// probed binary and its scope; without the scope it reads as a statement about
// the running build.
func TestOpencodePluginStatusTextScopesTheBuiltinAnswer(t *testing.T) {
	probe := func() (string, bool) { return "/pinned/opencode", true }
	got := opencodePluginStatusText(false, probe)
	if !strings.Contains(got, "/pinned/opencode") {
		t.Errorf("built-in status answer does not name the probed binary: %q", got)
	}
	if !strings.Contains(got, "not necessarily the running build") {
		t.Errorf("built-in status answer does not limit itself to the probed binary: %q", got)
	}
}

// The announcement and the deletion loop share one list, which makes that list
// the authority for what RemoveAll receives. It holds plain entry names only; a
// value that resolves outside <home>/.config/opencode/plugins must be refused
// rather than followed.
func TestRemoveOpencodePluginRefusesAnEntryThatEscapesThePluginDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	pluginsDir := filepath.Join(home, ".config", "opencode", "plugins")
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// A file next to the plugin directory: "../neighbour" would reach it.
	neighbour := filepath.Join(home, ".config", "opencode", "neighbour")
	if err := os.WriteFile(neighbour, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}

	previous := opencodePluginDirEntries
	opencodePluginDirEntries = append(append([]string{}, previous...), "../neighbour")
	t.Cleanup(func() { opencodePluginDirEntries = previous })

	if err := removeOpencodePlugin(home); err == nil {
		t.Error("removeOpencodePlugin accepted an entry that leaves the plugin directory")
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Errorf("an entry outside the plugin directory was removed: %v", err)
	}
}

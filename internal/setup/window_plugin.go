package setup

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/carsteneu/yesmem/plugins"
)

// InstallWindowPlugin extracts the bundled v2 TUI plugin to
// ~/.config/opencode/plugins/yesmem-window/tui.tsx if missing or outdated
// (SHA256 comparison). opencode auto-discovers plugins in that directory and
// hot-reloads them into every running TUI, which then register the session
// their window shows with the daemon (register_window) — the source
// spawn_agent uses to open agent windows on the caller's desktop.
// Returns the number of files updated. Skips (0, nil) when the user has no
// ~/.config/opencode, so Claude-only users get nothing created.
func InstallWindowPlugin(home string) (int, error) {
	cfgDir := filepath.Join(home, ".config", "opencode")
	if fi, err := os.Stat(cfgDir); err != nil || !fi.IsDir() {
		return 0, nil
	}

	data, err := plugins.WindowPlugin.ReadFile("yesmem-window/tui.tsx")
	if err != nil {
		return 0, fmt.Errorf("read embedded window plugin: %w", err)
	}

	dst := filepath.Join(home, opencodePluginsRelDir, "yesmem-window", "tui.tsx")
	if existing, err := os.ReadFile(dst); err == nil {
		if fmt.Sprintf("%x", sha256.Sum256(existing)) == fmt.Sprintf("%x", sha256.Sum256(data)) {
			return 0, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return 0, fmt.Errorf("create plugin dir: %w", err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		return 0, fmt.Errorf("write window plugin: %w", err)
	}
	return 1, nil
}

package setup

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/carsteneu/yesmem/plugins"
)

// ThemeDir is the directory opencode reads custom TUI themes from, relative to
// ~/.config/opencode.
const ThemeDir = "themes"

// InstallThemes extracts the bundled agent theme family to
// ~/.config/opencode/themes/ if a file is missing or outdated (SHA256
// comparison). opencode discovers every *.json in that directory and offers it
// as a theme name, which is how a spawned agent's TUI gets its type accent.
// Returns the number of files updated. Skips (0, nil) while the user has no
// ~/.config/opencode, so a Claude-only install that never created that
// directory gets nothing created.
func InstallThemes(home string) (int, error) {
	cfgDir := filepath.Join(home, ".config", "opencode")
	if fi, err := os.Stat(cfgDir); err != nil || !fi.IsDir() {
		return 0, nil
	}

	entries, err := fs.ReadDir(plugins.AgentThemes, "themes")
	if err != nil {
		return 0, fmt.Errorf("read embedded themes: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	updated := 0
	dstDir := filepath.Join(cfgDir, ThemeDir)
	for _, name := range names {
		data, err := plugins.AgentThemes.ReadFile("themes/" + name)
		if err != nil {
			return updated, fmt.Errorf("read embedded theme %s: %w", name, err)
		}
		dst := filepath.Join(dstDir, name)
		if existing, err := os.ReadFile(dst); err == nil && sha256.Sum256(existing) == sha256.Sum256(data) {
			continue
		}
		if err := os.MkdirAll(dstDir, 0755); err != nil {
			return updated, fmt.Errorf("create theme dir: %w", err)
		}
		// tmp+rename: a TUI may read the directory at any moment, and a
		// half-written file would fail its JSON.parse for every theme of that
		// session, not just this one.
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, data, 0644); err != nil {
			return updated, fmt.Errorf("write theme %s: %w", name, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return updated, fmt.Errorf("install theme %s: %w", name, err)
		}
		updated++
	}
	return updated, nil
}

// RemoveThemes deletes the bundled theme files from
// ~/.config/opencode/themes/. Only the files yesmem installs are removed — a
// theme the user added to that directory stays.
func RemoveThemes(home string) error {
	entries, err := fs.ReadDir(plugins.AgentThemes, ThemeDir)
	if err != nil {
		return fmt.Errorf("read embedded themes: %w", err)
	}
	dstDir := filepath.Join(home, ".config", "opencode", ThemeDir)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dstDir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove theme %s: %w", entry.Name(), err)
		}
	}
	return nil
}

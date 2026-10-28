// Package opencode holds helpers shared across internal/setup and
// internal/proxy for resolving opencode's config file location.
//
// It exists to avoid an import cycle: internal/setup imports daemon+httpapi,
// internal/proxy imports daemon, and both need to know which opencode config
// file opencode actually reads. Putting the helper in either of those
// packages would force the other into a cycle.
package opencode

import (
	"os"
	"path/filepath"
)

// ConfigPath returns the path to the opencode config file opencode actually
// reads, mirroring opencode's own globalConfigFile() priority:
//
//	opencode.jsonc → opencode.json → config.json
//
// Falls back to opencode.jsonc (opencode's preferred format for fresh
// installs) when none exist. yesmem MUST write to the same file opencode
// reads, otherwise yesmem's provider blocks, MCP config, and model defaults
// are silently ignored. This was the root cause of the "LLM call failed:
// opencode error: exit status 1" bug on fresh opencode installs where only
// opencode.jsonc existed.
//
// The return value is deterministic given the same filesystem state — no
// time randomness, no env vars beyond os.Stat. The function does NOT create
// the file or any directories; callers handle creation.
//
// XDG_CONFIG_HOME is deliberately NOT consulted. opencode itself resolves its
// config directory through XDG, so honouring it here would be the first half of
// a migration: the plugin, theme and command directories in internal/setup and
// internal/daemon are hardcoded to ~/.config/opencode, and the daemon runs from a
// systemd unit whose environment does not carry the shell's XDG_CONFIG_HOME. Both
// halves would then disagree about the file they read and write (YM-376, PRX-P47,
// item 4 needs one coherent migration instead).
func ConfigPath(home string) string {
	return ConfigPathInDir(filepath.Join(home, ".config", "opencode"))
}

// ConfigPathInDir is ConfigPath for an explicit config directory. A host can point
// XDG_CONFIG_HOME somewhere else, and a caller that masks or reads that directory
// has to resolve the file the same way opencode does.
func ConfigPathInDir(dir string) string {
	for _, name := range []string{"opencode.jsonc", "opencode.json", "config.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return filepath.Join(dir, name)
		}
	}
	return filepath.Join(dir, "opencode.jsonc")
}

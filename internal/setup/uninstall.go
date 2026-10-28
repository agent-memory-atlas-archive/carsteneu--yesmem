package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/skills"
)

// UserHome resolves the home directory every install, update and status path is
// based on, and it refuses an unusable value. The error of os.UserHomeDir is not
// optional: with an empty HOME each target becomes a path relative to the working
// directory, and "/" as HOME puts the data directory beside the filesystem root.
// Both were reachable while four call sites dropped the error of
// os.UserHomeDir (YM-350).
func UserHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) == string(filepath.Separator) {
		return "", fmt.Errorf("refusing to run with home directory %q: not a usable absolute path", home)
	}
	return filepath.Clean(home), nil
}

// archiveOutsideDataDir reports whether the archive base lies outside the data dir,
// which the delete-data step of an uninstall does not cover. Resolved with
// filepath.Rel rather than a string prefix: "/data/../keep/archive" shares the
// prefix with "/data" but lives somewhere else.
func archiveOutsideDataDir(archiveDir, dataDir string) bool {
	base, err := filepath.Abs(dataDir)
	if err != nil {
		return true
	}
	target, err := filepath.Abs(archiveDir)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// uninstallPreviewLines is the announcement Uninstall prints before it asks for
// confirmation. It lives apart from the printing so it can be read in a test:
// Uninstall reads its answers from a package-level bufio.Reader, which a test
// cannot replace.
func uninstallPreviewLines() []string {
	// Best effort: the announcement names what the sweep will remove, read from
	// the same list. A bundle read that fails leaves both lists empty; the sweep
	// itself reports that failure instead of hiding it.
	opencodeCommands, _ := bundledOpencodeCommandNames()
	lines := []string{
		"",
		"  YesMem Uninstall",
		"  ================",
		"",
		"  This will remove:",
		"  ✓ MCP server registration from settings.json + claude.json",
		"  ✓ SessionStart hook",
		"  ✓ UserPromptSubmit micro-reminder",
		"  ✓ PreToolUse hook (gotcha warnings)",
		"  ✓ PostToolUseFailure hook (auto-learn)",
		"  ✓ cleanupPeriodDays setting",
		"  ✓ autoCompactEnabled (restore previous value)",
		"  ✓ ANTHROPIC_BASE_URL from settings.json env + shell profiles",
		"  ✓ Hook script (~/.claude/hooks/yesmem-briefing.sh)",
		"  ✓ .mcp.json entries",
		"  ✓ Codex provider + MCP entries in ~/.codex/config.toml",
		"  ✓ Codex instructions (~/.codex/instructions/yesmem.md)",
		"  ✓ Opencode plugins under ~/" + filepath.ToSlash(opencodePluginsRelDir) + "/ (" + strings.Join(opencodePluginDirEntries, ", ") + ")",
		"  ✓ Opencode themes yesmem installed under ~/.config/opencode/" + ThemeDir + "/",
		"  ✓ Opencode v1 plugin source, while the opencode config is rewritable (~/" + filepath.ToSlash(filepath.Dir(v1PluginPersistentRelPath)) + ")",
		"  ✓ Opencode settings (provider, MCP, compaction in opencode.json)",
		"  ✓ Bundled commands under ~/.claude/commands/ (" + strings.Join(bundledCommandNames(), ", ") + ")",
		"  ✓ Bundled skills under ~/.claude/skills/ (" + strings.Join(bundledSkillNames(), ", ") + ")",
		"  ✓ Bundled opencode commands under ~/" + filepath.ToSlash(opencodeCommandsRelDir) + "/ (" + strings.Join(opencodeCommands, ", ") + ")",
	}
	if runtime.GOOS == "linux" {
		lines = append(lines, "  ✓ systemd user services (daemon + proxy)")
	} else if runtime.GOOS == "darwin" {
		lines = append(lines, "  ✓ launchd plists (daemon + proxy)")
	}
	return append(lines, "  ✓ yesmem binary", "")
}

// Uninstall removes all YesMem registrations and optionally data.
func Uninstall() error {
	home, err := UserHome()
	if err != nil {
		return err
	}

	for _, line := range uninstallPreviewLines() {
		fmt.Println(line)
	}

	deleteData := promptYesNo("Also delete all data (~/.claude/yesmem/)?", false)
	fmt.Println()

	if !promptYesNo("Proceed with uninstall?", false) {
		fmt.Println("  Cancelled.")
		return nil
	}
	fmt.Println()

	// Kill daemon + proxy
	fmt.Print("  Stopping daemon... ")
	exec.Command("pkill", "-f", "yesmem daemon").Run()
	fmt.Println("✓")
	fmt.Print("  Stopping proxy... ")
	exec.Command("pkill", "-f", "yesmem proxy").Run()
	fmt.Println("✓")

	// Remove from settings.json
	fmt.Print("  Cleaning settings.json... ")
	if err := cleanSettingsJSON(home); err != nil {
		fmt.Printf("⚠ %v\n", err)
	} else {
		fmt.Println("✓")
	}

	// Remove hook script
	fmt.Print("  Removing hook script... ")
	os.Remove(filepath.Join(home, ".claude", "hooks", "yesmem-briefing.sh"))
	fmt.Println("✓")

	// Remove bundled commands installed by YesMem
	fmt.Print("  Removing bundled commands... ")
	removeBundledCommands(home)
	fmt.Println("✓")

	// Remove bundled skills installed by YesMem. A same-named directory of the
	// user's own is kept, and the run names it: a silent skip looks like a
	// forgotten sweep.
	fmt.Print("  Removing bundled skills... ")
	keptSkills, skillsErr := removeBundledSkills(home)
	if skillsErr != nil {
		fmt.Printf("⚠ %v\n", skillsErr)
	} else {
		fmt.Println("✓")
	}
	for _, dir := range keptSkills {
		fmt.Printf("  Kept %s (content differs from the bundled skill)\n", dir)
	}

	// Remove bundled opencode commands installed by YesMem
	fmt.Print("  Removing bundled opencode commands... ")
	if err := removeBundledOpencodeCommands(home); err != nil {
		fmt.Printf("⚠ %v\n", err)
	} else {
		fmt.Println("✓")
	}

	// Remove .mcp.json entries
	fmt.Print("  Cleaning .mcp.json... ")
	cleanMCPJSON(filepath.Join(home, ".mcp.json"))
	fmt.Println("✓")

	// Remove MCP from ~/.claude.json + restore primaryApiKey
	fmt.Print("  Cleaning claude.json... ")
	dataDir := filepath.Join(home, ".claude", "yesmem")
	restorePrimaryApiKeyFromState(dataDir, home)
	if err := cleanClaudeJSON(home); err != nil {
		fmt.Printf("⚠ %v\n", err)
	} else {
		fmt.Println("✓")
	}

	fmt.Print("  Cleaning Codex config... ")
	if err := removeCodexSetup(home); err != nil {
		fmt.Printf("⚠ %v\n", err)
	} else {
		fmt.Println("✓")
	}

	fmt.Print("  Cleaning Opencode config... ")
	if err := removeOpencodePlugin(home); err != nil {
		fmt.Printf("⚠ %v\n", err)
	} else {
		fmt.Println("✓")
	}

	// Remove yesmem permissions from project-local settings.local.json files
	fmt.Print("  Cleaning project settings... ")
	cleanProjectLocalSettings(home)
	fmt.Println("✓")

	// Remove YesMem auto-generated section from project MEMORY.md files
	fmt.Print("  Cleaning project memory files... ")
	cleanProjectMemoryFiles(claudeProjectsDirFromConfig(dataDir))
	fmt.Println("✓")

	// Remove ANTHROPIC_BASE_URL from shell profiles
	fmt.Print("  Cleaning shell profiles... ")
	if err := removeProxyFromShellProfiles(home); err != nil {
		fmt.Printf("⚠ %v\n", err)
	} else {
		fmt.Println("✓")
	}

	// Remove systemd/launchd
	if runtime.GOOS == "linux" {
		fmt.Print("  Removing systemd services... ")
		for _, svc := range []string{"yesmem", "yesmem-proxy"} {
			exec.Command("systemctl", "--user", "stop", svc).Run()
			exec.Command("systemctl", "--user", "disable", svc).Run()
			os.Remove(filepath.Join(home, ".config", "systemd", "user", svc+".service"))
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		fmt.Println("✓")
	} else if runtime.GOOS == "darwin" {
		fmt.Print("  Removing launchd plists... ")
		for _, name := range []string{"com.yesmem.daemon", "com.yesmem.proxy"} {
			plist := filepath.Join(home, "Library", "LaunchAgents", name+".plist")
			exec.Command("launchctl", "unload", plist).Run()
			os.Remove(plist)
		}
		fmt.Println("✓")
	}

	// Remove PID + socket + session tracking files
	fmt.Print("  Cleaning runtime files... ")
	os.Remove(filepath.Join(dataDir, "daemon.sock"))
	os.Remove(filepath.Join(dataDir, "daemon.pid"))
	os.RemoveAll(filepath.Join(dataDir, "sessions"))
	// The restricted job socket, in the runtime dir beside the data dir (YM-276;
	// the name mirrors daemon.RuntimeDir).
	os.Remove(filepath.Join(dataDir+"-run", "job.sock"))
	fmt.Println("✓")

	// Optionally delete data
	if deleteData {
		// paths.archive can point outside the data dir. Resolve it before the data
		// dir (with config.yaml inside it) is gone, and say what stays: a removal
		// reported as complete would otherwise leave a full transcript archive
		// behind. It is not deleted, because that path may be shared.
		archiveDir := ""
		if uninstallCfg, err := config.Load(filepath.Join(dataDir, "config.yaml")); err == nil {
			archiveDir = uninstallCfg.ArchiveDir(dataDir)
		}
		fmt.Print("  Deleting all data... ")
		os.RemoveAll(dataDir)
		// The job log dirs live outside the data dir, so they need their own sweep.
		os.RemoveAll(dataDir + "-run")
		fmt.Println("✓")
		if archiveDir != "" && archiveOutsideDataDir(archiveDir, dataDir) {
			fmt.Printf("  Left in place: transcript archive outside the data dir: %q\n", archiveDir)
		}
		// Clean up backup file created during install
		backupPath := filepath.Join(home, ".claude.json.bak")
		if _, err := os.Stat(backupPath); err == nil {
			fmt.Print("  Removing claude.json backup... ")
			os.Remove(backupPath)
			fmt.Println("✓")
		}
	}

	// Delete binary (self-delete — works on Linux, inode stays open until process exits)
	fmt.Print("  Removing binary... ")
	binaryPath, err := os.Executable()
	if err == nil {
		binaryPath, _ = filepath.EvalSymlinks(binaryPath)
		// The sweep only runs when the binary is really gone: a removal that
		// failed (root-owned binary, uninstall as a user) would otherwise delete
		// the PATH entry of an installation that still works.
		if removeErr := os.Remove(binaryPath); removeErr == nil || os.IsNotExist(removeErr) {
			removeStaleBinaryLinks(home, binaryPath)
		} else {
			log.Printf("uninstall: remove binary %s: %v", binaryPath, removeErr)
		}
	}
	fmt.Println("✓")

	fmt.Println()
	fmt.Println("  ══════════════════════════════════════")
	fmt.Println("  YesMem uninstalled.")
	if !deleteData {
		fmt.Printf("  Data preserved at %s\n", dataDir)
		fmt.Println("  Run with data deletion to fully remove.")
	}
	fmt.Println("  ══════════════════════════════════════")
	fmt.Println()

	return nil
}

// binaryLinkDirs are the user-writable directories a yesmem link can sit in.
// System directories are not swept: removing a link there needs root, and the
// uninstall does not claim to.
var binaryLinkDirs = []string{
	filepath.Join(".local", "bin"),
	"bin",
	filepath.Join("go", "bin"),
}

// removeStaleBinaryLinks removes symlinks in the user's bin directories that
// point at the binary this run deleted. os.Executable reports the resolved
// target, so a link the user invokes yesmem through is invisible to the removal
// above and stays behind as a PATH entry that exists but cannot start (YM-350).
// A link to anything else, and a regular file of the same name, are left alone.
func removeStaleBinaryLinks(home, target string) {
	if target == "" || home == "" {
		return
	}
	want := filepath.Clean(target)
	for _, rel := range binaryLinkDirs {
		dir := filepath.Join(home, rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			linkPath := filepath.Join(dir, entry.Name())
			dest, err := os.Readlink(linkPath)
			if err != nil {
				continue
			}
			if !filepath.IsAbs(dest) {
				dest = filepath.Join(dir, dest)
			}
			if filepath.Clean(dest) != want {
				continue
			}
			if err := os.Remove(linkPath); err != nil && !os.IsNotExist(err) {
				log.Printf("uninstall: remove stale link %s: %v", linkPath, err)
			}
		}
	}
}

// cleanSettingsJSON removes every trace of the install from settings.json.
func cleanSettingsJSON(home string) error {
	dataDir := filepath.Join(home, ".claude", "yesmem")
	settings, err := readSettingsJSON(home)
	if err != nil {
		return err
	}

	// The path yesmem registered for its MCP server is the path it wrote into
	// every hook command. It identifies our handlers even when the binary name
	// does not carry the word "yesmem" — read before the entry is deleted below.
	installBinary := yesmemBinaryFromSettings(settings)

	// Remove mcpServers.yesmem
	if mcp, ok := settings["mcpServers"].(map[string]any); ok {
		delete(mcp, "yesmem")
		if len(mcp) == 0 {
			delete(settings, "mcpServers")
		}
	}

	// Remove cleanupPeriodDays (only if it's our value)
	if val, ok := settings["cleanupPeriodDays"].(float64); ok && val == 99999 {
		delete(settings, "cleanupPeriodDays")
	}

	// Restore autoCompactEnabled to pre-install state
	if state := loadPreInstallState(dataDir); state != nil {
		if prev, exists := state["autoCompactEnabled"]; exists {
			if prev == nil {
				delete(settings, "autoCompactEnabled") // was absent before install
			} else {
				settings["autoCompactEnabled"] = prev
			}
		}
	} else {
		// No install-state.json → safe default: remove the key (let Claude Code use its default)
		delete(settings, "autoCompactEnabled")
	}

	// Restore ANTHROPIC_API_KEY to pre-install value (must run before removeProxyEnvVar)
	restoreAPIKeyFromState(dataDir, settings)

	// Replace proxy URL: bypass bridge for API-key users, remove for OAuth users
	removeProxyEnvVar(settings)

	// Remove yesmem MCP permissions
	cleanMCPPermissions(settings)

	// Remove statusLine if it's yesmem's
	cleanStatusLine(settings, installBinary)

	// Remove SessionStart hook entries containing yesmem
	if hooks, ok := settings["hooks"].(map[string]any); ok {
		removeYesmemHook(hooks, "SessionStart", installBinary)
		removeYesmemHook(hooks, "SessionEnd", installBinary)
		removeYesmemFromCommand(hooks, "UserPromptSubmit", installBinary)
		removeYesmemHook(hooks, "PreToolUse", installBinary)
		removeYesmemHook(hooks, "PostToolUseFailure", installBinary)
		removeYesmemHook(hooks, "PostToolUse", installBinary)
	}

	return writeSettingsJSON(home, settings)
}

// yesmemBinaryFromSettings returns the binary path yesmem registered for its MCP
// server. Every path that writes a hook writes it as that path plus a subcommand.
func yesmemBinaryFromSettings(settings map[string]any) string {
	mcp, ok := settings["mcpServers"].(map[string]any)
	if !ok {
		return ""
	}
	entry, ok := mcp["yesmem"].(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := entry["command"].(string)
	return cmd
}

// restoreAPIKeyFromState restores ANTHROPIC_API_KEY in env to pre-install value.
func restoreAPIKeyFromState(dataDir string, settings map[string]any) {
	state := loadPreInstallState(dataDir)
	env, ok := settings["env"].(map[string]any)
	if state == nil {
		if ok {
			delete(env, "ANTHROPIC_API_KEY")
		}
		return
	}
	prev, exists := state["envAPIKey"]
	if !exists {
		return
	}
	if prev == nil {
		if ok {
			delete(env, "ANTHROPIC_API_KEY")
		}
	} else if prevStr, ok := prev.(string); ok {
		if env == nil {
			env = map[string]any{}
			settings["env"] = env
		}
		env["ANTHROPIC_API_KEY"] = prevStr
	}
}

// restorePrimaryApiKeyFromState restores primaryApiKey in ~/.claude.json to pre-install value.
func restorePrimaryApiKeyFromState(dataDir, home string) {
	state := loadPreInstallState(dataDir)
	if state == nil {
		return
	}
	prev, exists := state["primaryApiKey"]
	if !exists {
		return
	}
	path := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return
	}
	if prev == nil {
		delete(cfg, "primaryApiKey")
	} else if prevStr, ok := prev.(string); ok {
		cfg["primaryApiKey"] = prevStr
	}
	if oa, ok := state["oauthAccount"]; ok && oa != nil {
		cfg["oauthAccount"] = oa
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	// The key goes back into a file that predates yesmem, so the mode literal
	// alone would keep whatever Claude Code left behind.
	if err := writeSecretFile(path, out); err != nil {
		fmt.Printf("  warning: could not tighten claude.json: %v\n", err)
	}
}

// removeYesmemHook removes entire hook entries that reference yesmem.
func removeYesmemHook(hooks map[string]any, hookName, binaryPath string) {
	entries, ok := hooks[hookName].([]any)
	if !ok {
		return
	}

	var filtered []any
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			filtered = append(filtered, entry)
			continue
		}
		isYesmem := false
		for _, h := range toHookSlice(m["hooks"]) {
			if cmd, ok := h["command"].(string); ok && isYesmemHandler(cmd, "", binaryPath) {
				isYesmem = true
			}
		}
		if !isYesmem {
			filtered = append(filtered, entry)
		}
	}

	if len(filtered) == 0 {
		delete(hooks, hookName)
	} else {
		hooks[hookName] = filtered
	}
}

// removeYesmemFromCommand removes yesmem from chained commands ("; yesmem micro-reminder").
func removeYesmemFromCommand(hooks map[string]any, hookName, binaryPath string) {
	entries, ok := hooks[hookName].([]any)
	if !ok {
		return
	}

	var kept []any
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			kept = append(kept, entry)
			continue
		}
		var hooksList []map[string]any
		for _, h := range toHookSlice(m["hooks"]) {
			cmd, ok := h["command"].(string)
			if !ok {
				hooksList = append(hooksList, h)
				continue
			}
			// Remove the yesmem segments from a command chain. A handler that
			// consisted only of yesmem — idle-tick registers as its own entry —
			// is dropped instead of being left with an empty command.
			var cleaned []string
			for _, p := range strings.Split(cmd, ";") {
				if !invokesYesmem(strings.TrimSpace(p), binaryPath) {
					cleaned = append(cleaned, p)
				}
			}
			joined := strings.TrimSpace(strings.Join(cleaned, ";"))
			if joined == "" {
				continue
			}
			h["command"] = joined
			hooksList = append(hooksList, h)
		}
		if len(hooksList) == 0 {
			continue
		}
		m["hooks"] = toAnySlice(hooksList)
		kept = append(kept, m)
	}

	if len(kept) == 0 {
		delete(hooks, hookName)
	} else {
		hooks[hookName] = kept
	}
}

func cleanMCPJSON(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return
	}
	mcp, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return
	}
	if _, had := mcp["yesmem"]; !had {
		return
	}
	delete(mcp, "yesmem")
	if len(mcp) == 0 {
		delete(cfg, "mcpServers")
	}
	// The file is shared: it can carry other servers and other top-level keys
	// (project entries among them). Removing it as soon as the yesmem server is
	// gone deleted foreign configuration with it, so it goes only when nothing
	// else is left.
	if len(cfg) == 0 {
		os.Remove(path)
		return
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("uninstall: marshal %s: %v", path, err)
		return
	}
	// A write that fails leaves the yesmem server in the file while the run
	// reports a cleaned config, so it is named at least.
	if err := os.WriteFile(path, out, 0644); err != nil {
		log.Printf("uninstall: write %s: %v", path, err)
	}
}

// cleanClaudeJSON removes the yesmem MCP entry from ~/.claude.json.
func cleanClaudeJSON(home string) error {
	path := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parse claude.json: %w", err)
	}
	mcpServers, ok := config["mcpServers"].(map[string]any)
	if ok {
		delete(mcpServers, "yesmem")
		if len(mcpServers) == 0 {
			delete(config, "mcpServers")
		}
	}
	// Remove yesmem skill usage entries
	if skillUsage, ok := config["skillUsage"].(map[string]any); ok {
		for k := range skillUsage {
			if isYesmemSkillUsageKey(k) {
				delete(skillUsage, k)
			}
		}
		if len(skillUsage) == 0 {
			delete(config, "skillUsage")
		}
	}
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	// The file predates yesmem on most installs, so the mode literal alone would
	// keep whatever Claude Code left behind.
	return writeSecretFile(path, out)
}

// yesmemSkillUsageNames holds the skill names yesmem installs, read once from
// the embedded bundle. skillUsage is keyed by skill name, so ownership is a
// name match: a substring test on "yesmem" deleted a user's own
// my-yesmem-helper and missed every yesmem skill that carries no prefix
// (yesloop, reddit, security-review).
var (
	yesmemSkillUsageOnce  sync.Once
	yesmemSkillUsageNames map[string]bool
)

func isYesmemSkillUsageKey(key string) bool {
	yesmemSkillUsageOnce.Do(func() {
		yesmemSkillUsageNames = map[string]bool{}
		entries, err := skills.BundledSkills.ReadDir("bundled-skills")
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				yesmemSkillUsageNames[e.Name()] = true
			}
		}
	})
	if yesmemSkillUsageNames[key] {
		return true
	}
	// A cap-backed skill records usage per tool, so the reddit skill writes
	// reddit_fetch and reddit_search rather than its directory name.
	if i := strings.IndexByte(key, '_'); i > 0 && yesmemSkillUsageNames[key[:i]] {
		return true
	}
	// Skills an older yesmem shipped are gone from the bundle, but their usage
	// rows are still ours; the family has carried this prefix throughout.
	return strings.HasPrefix(key, "yesmem-")
}

// removeProxyFromShellProfiles removes the ANTHROPIC_BASE_URL export lines
// and YesMem PATH entries that were added during install.
func removeProxyFromShellProfiles(home string) error {
	for _, rcFile := range []string{".bashrc", ".zshrc", ".profile"} {
		rcPath := filepath.Join(home, rcFile)
		data, err := os.ReadFile(rcPath)
		if err != nil {
			continue
		}
		content := string(data)
		if !strings.Contains(content, "ANTHROPIC_BASE_URL") && !strings.Contains(content, "# YesMem binary") {
			continue
		}
		lines := strings.Split(content, "\n")
		var cleaned []string
		for i := 0; i < len(lines); i++ {
			trimmed := strings.TrimSpace(lines[i])
			// Skip the proxy comment line and the export line
			if trimmed == "# YesMem infinite-thread proxy" {
				// Also skip the next line if it's the export
				if i+1 < len(lines) && strings.Contains(lines[i+1], "ANTHROPIC_BASE_URL") {
					i++
				}
				continue
			}
			// Catch standalone export line (without preceding comment)
			if strings.Contains(trimmed, "export ANTHROPIC_BASE_URL=http://localhost:9099") {
				continue
			}
			// Skip the PATH comment line and the export line
			if trimmed == "# YesMem binary" {
				if i+1 < len(lines) && strings.Contains(lines[i+1], "export PATH=") {
					i++
				}
				continue
			}
			cleaned = append(cleaned, lines[i])
		}
		// Remove trailing empty lines that were left behind
		for len(cleaned) > 0 && strings.TrimSpace(cleaned[len(cleaned)-1]) == "" {
			cleaned = cleaned[:len(cleaned)-1]
		}
		cleaned = append(cleaned, "") // ensure final newline
		if err := os.WriteFile(rcPath, []byte(strings.Join(cleaned, "\n")), 0644); err != nil {
			return fmt.Errorf("write %s: %w", rcFile, err)
		}
	}
	return nil
}

// isPlainEntryName reports whether an embedded bundle entry names a single
// file or directory. The sweep below hands these names to os.Remove and
// os.RemoveAll, and a name that carries a path separator would reach outside
// the target directory, so every list entry passes through here.
func isPlainEntryName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '\\') {
		return false
	}
	return name == filepath.Base(name)
}

// bundledCommandNames lists the command files yesmem installs into
// ~/.claude/commands. The announcement and the deletion read the same list, so
// the text cannot fall behind what the sweep removes.
func bundledCommandNames() []string {
	entries, err := skills.BundledCommands.ReadDir(".")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || !isPlainEntryName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}

// bundledSkillNames lists the skill directories yesmem installs into
// ~/.claude/skills, read from the same bundle InstallBundledSkills copies.
func bundledSkillNames() []string {
	entries, err := skills.BundledSkills.ReadDir("bundled-skills")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || !isPlainEntryName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}

// removeBundledCommands removes YesMem-installed skill files from ~/.claude/commands/.
// Only removes files that match names in the embedded skills directory.
func removeBundledCommands(home string) {
	cmdDir := filepath.Join(home, ".claude", "commands")
	for _, name := range bundledCommandNames() {
		os.Remove(filepath.Join(cmdDir, name))
	}
}

// removeBundledSkills removes YesMem-installed skill directories from ~/.claude/skills/
// and returns the ones it kept. A directory of the same name is removed only when
// it holds exactly the bundled files: the directory is shared with the user, and
// an uninstall used to delete a skill of their own that happened to carry a
// bundled name (YM-350).
func removeBundledSkills(home string) (kept []string, err error) {
	skillsDir := filepath.Join(home, ".claude", "skills")
	var errs []error
	for _, name := range bundledSkillNames() {
		dir := filepath.Join(skillsDir, name)
		ours, lookErr := skillDirIsBundledCopy(name, dir)
		if lookErr != nil {
			// A missing directory is the normal case: nothing was installed.
			if !os.IsNotExist(lookErr) {
				errs = append(errs, lookErr)
			}
			continue
		}
		if !ours {
			kept = append(kept, dir)
			continue
		}
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			errs = append(errs, removeErr)
		}
	}
	return kept, errors.Join(errs...)
}

// skillDirIsBundledCopy reports whether dir holds exactly the bundled files of
// that skill, byte for byte. An extra file, a missing one or a changed one makes
// the directory foreign: a stale install that lost a file is kept rather than
// guessed at, because deleting a user's own work is the failure this guards.
//
// Only the top level is compared. Bundled skills are flat by construction (the
// installer copies files, not subdirectories); a bundle that ever ships one would
// need this function to recurse, otherwise its installed copy is never "ours".
func skillDirIsBundledCopy(name, dir string) (bool, error) {
	entries, err := skills.BundledSkills.ReadDir(filepath.Join("bundled-skills", name))
	if err != nil {
		return false, err
	}
	bundled := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		bundled[e.Name()] = true
	}
	if len(bundled) == 0 {
		return false, fmt.Errorf("bundle names no file for skill %q", name)
	}
	present, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	if len(present) != len(bundled) {
		return false, nil
	}
	for _, e := range present {
		if e.IsDir() || !bundled[e.Name()] {
			return false, nil
		}
		want, err := skills.BundledSkills.ReadFile(filepath.Join("bundled-skills", name, e.Name()))
		if err != nil {
			return false, err
		}
		got, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return false, err
		}
		if !bytes.Equal(got, want) {
			return false, nil
		}
	}
	return true, nil
}

// opencodeCommandsRelDir is where InstallOpencodeCommands puts the bundled
// opencode commands and where the uninstall sweep looks for them. One value for
// both sides: two derivations of the same path drift apart silently (YM-359).
// The directory is shared with the user, so the sweep only removes the names
// the bundle carries.
var opencodeCommandsRelDir = filepath.Join(".config", "opencode", "commands")

// bundledOpencodeCommandNames lists the command files yesmem installs into
// ~/.config/opencode/commands, read from the bundle InstallOpencodeCommands
// copies. It covers every non-directory entry the installer writes, and refuses
// a name that is not a plain base name: these names are handed to os.Remove.
//
// The read error is returned, not flattened to an empty list: an empty list
// would let the sweep delete nothing and still report success.
func bundledOpencodeCommandNames() ([]string, error) {
	entries, err := skills.BundledOpencodeCommands.ReadDir("bundled-commands/opencode")
	if err != nil {
		return nil, fmt.Errorf("read bundled opencode commands: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !isPlainEntryName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// removeBundledOpencodeCommands removes the opencode commands YesMem installed
// from ~/.config/opencode/commands/. A command of the user's own that sits in
// the same directory is not named by the bundle and stays.
//
// A remove that fails is returned rather than dropped: the file would stay
// behind, which is the state this sweep exists to clear, and a silent success
// line would report the opposite. A missing file is not a failure.
func removeBundledOpencodeCommands(home string) error {
	cmdsDir := filepath.Join(home, opencodeCommandsRelDir)
	// A commands directory that is itself a symlink would carry the unlink into
	// whatever it points at. The plugin installer refuses the same shape.
	if fi, err := os.Lstat(cmdsDir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("opencode commands dir %s is a symlink, refusing to sweep it", cmdsDir)
	}
	names, err := bundledOpencodeCommandNames()
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names {
		if err := os.Remove(filepath.Join(cmdsDir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// mcpToolNames lists all yesmem MCP tool names for permission cleanup.
var mcpToolNames = []string{
	"search", "deep_search", "hybrid_search", "remember", "reopen", "resolve", "resolve_by_text",
	"get_learnings", "get_session", "get_compacted_stubs", "get_project_profile",
	"get_self_feedback", "get_persona", "set_persona", "get_pins", "pin", "unpin",
	"get_coverage", "related_to_file", "list_projects", "project_summary",
	"scratchpad_write", "scratchpad_read", "scratchpad_list", "scratchpad_delete",
	"ingest_docs", "docs_search", "list_docs", "remove_docs",
	"send_to", "broadcast", "spawn_agent", "stop_agent", "stop_all_agents",
	"list_agents", "get_agent", "relay_agent", "resume_agent",
	"set_plan", "update_plan", "get_plan", "complete_plan",
	"set_config", "get_config", "expand_context", "whoami",
	"update_agent_status", "skip_indexing", "quarantine_session", "unquarantine_session",
	"query_facts", "relate_learnings",
}

// cleanMCPPermissions removes all mcp__yesmem__* entries from permissions.allow.
func cleanMCPPermissions(settings map[string]any) {
	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		return
	}
	allow, ok := perms["allow"].([]any)
	if !ok {
		return
	}
	cleaned := make([]any, 0)
	for _, v := range allow {
		s, ok := v.(string)
		if !ok || !strings.HasPrefix(s, "mcp__yesmem__") {
			cleaned = append(cleaned, v)
		}
	}
	perms["allow"] = cleaned
}

// cleanStatusLine removes the statusLine entry if it belongs to yesmem.
func cleanStatusLine(settings map[string]any, binaryPath string) {
	sl, ok := settings["statusLine"].(map[string]any)
	if !ok {
		return
	}
	cmd, _ := sl["command"].(string)
	if isYesmemHandler(cmd, "", binaryPath) {
		delete(settings, "statusLine")
	}
}

// cleanProjectLocalSettings removes mcp__yesmem__* from permissions.allow
// in each project's .claude/settings.local.json file.
func cleanProjectLocalSettings(home string) {
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return
	}
	var claudeJSON map[string]any
	if json.Unmarshal(data, &claudeJSON) != nil {
		return
	}
	projects, ok := claudeJSON["projects"].(map[string]any)
	if !ok {
		return
	}
	for projPath := range projects {
		localPath := filepath.Join(projPath, ".claude", "settings.local.json")
		sdata, err := os.ReadFile(localPath)
		if err != nil {
			continue
		}
		var settings map[string]any
		if json.Unmarshal(sdata, &settings) != nil {
			continue
		}
		perms, ok := settings["permissions"].(map[string]any)
		if !ok {
			continue
		}
		allow, ok := perms["allow"].([]any)
		if !ok {
			continue
		}
		var cleaned []any
		for _, v := range allow {
			s, ok := v.(string)
			if !ok || !strings.HasPrefix(s, "mcp__yesmem__") {
				cleaned = append(cleaned, v)
			}
		}
		if len(cleaned) == len(allow) {
			continue
		}
		perms["allow"] = cleaned
		out, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			continue
		}
		os.WriteFile(localPath, out, 0644)
	}
}

const yesmemMemoryMarker = "# --- YesMem Auto-Generated"

// cleanProjectMemoryFiles removes the YesMem auto-generated section from each
// project's MEMORY.md below projectsDir.
func cleanProjectMemoryFiles(projectsDir string) {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		memPath := filepath.Join(projectsDir, e.Name(), "memory", "MEMORY.md")
		data, err := os.ReadFile(memPath)
		if err != nil {
			continue
		}
		content := string(data)
		idx := strings.Index(content, yesmemMemoryMarker)
		if idx < 0 {
			continue
		}
		cleaned := strings.TrimRight(content[:idx], " \t\n\r")
		if cleaned != "" {
			cleaned += "\n"
		}
		os.WriteFile(memPath, []byte(cleaned), 0644)
	}
}

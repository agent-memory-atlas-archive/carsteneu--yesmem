package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readSettingsJSON reads ~/.claude/settings.json as a map.
func readSettingsJSON(home string) (map[string]any, error) {
	path := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parse settings.json: %w", err)
	}
	return settings, nil
}

// writeSettingsJSON writes the settings map back to ~/.claude/settings.json.
func writeSettingsJSON(home string, settings map[string]any) error {
	path := filepath.Join(home, ".claude", "settings.json")
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return writeSecretFile(path, data)
}

// writeSecretFile writes data to a file only its owner can read. The mode in
// os.WriteFile applies on creation only, so a file an earlier version created
// world-readable keeps that mode — and the env block of settings.json carries
// ANTHROPIC_API_KEY — unless the file is chmodded. Tightening an existing file
// happens before the write, so the new content never sits under the old mode; the
// chmod after it also covers a file the umask narrowed on creation. Unlike
// hardenInstallStateFile, a failing chmod is reported: the caller should know that
// a file with a key in it stayed readable.
func writeSecretFile(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

// claudeProxyBaseURL is the yesmem proxy endpoint written into the
// settings.json env block while yesmem is installed.
const claudeProxyBaseURL = "http://localhost:9099"

// savePreInstallState persists settings values that install will overwrite,
// so uninstall can restore them. Saved to dataDir/install-state.json.
// Runs only while settings.json is untouched by yesmem: on a re-run the values
// in it are yesmem's own, so capturing them would lose the real pre-install state.
func savePreInstallState(dataDir string, settings map[string]any, home string) {
	statePath := filepath.Join(dataDir, "install-state.json")
	if yesmemApplied(settings) {
		// Nothing to capture, but tighten a file an older version left world-readable.
		hardenInstallStateFile(dataDir)
		return
	}
	state := map[string]any{}
	if val, ok := settings["autoCompactEnabled"]; ok {
		state["autoCompactEnabled"] = val
	} else {
		state["autoCompactEnabled"] = nil // was absent → remove on uninstall
	}
	// Preserve existing ANTHROPIC_API_KEY from env block
	if env, ok := settings["env"].(map[string]any); ok {
		if val, ok := env["ANTHROPIC_API_KEY"]; ok {
			state["envAPIKey"] = val
		} else {
			state["envAPIKey"] = nil
		}
	} else {
		state["envAPIKey"] = nil
	}
	// Preserve existing primaryApiKey from ~/.claude.json
	if home != "" {
		if data, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
			var cfg map[string]any
			if json.Unmarshal(data, &cfg) == nil {
				if k, ok := cfg["primaryApiKey"].(string); ok {
					state["primaryApiKey"] = k
				} else {
					state["primaryApiKey"] = nil
				}
				if oa, ok := cfg["oauthAccount"]; ok {
					state["oauthAccount"] = oa
				}
			}
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	// A failed write is not fatal here, and the chmod below is the only thing
	// that repairs the mode of a file an older version created.
	_ = os.WriteFile(statePath, data, 0600)
	// os.WriteFile honours perm only when it creates the file, so an existing
	// 0644 file stays 0644 unless it is chmodded explicitly.
	hardenInstallStateFile(dataDir)
}

// hardenInstallStateFile narrows install-state.json to owner-only. It holds
// primaryApiKey and the user's API key, so a file written by an older version
// has to be tightened even when nothing is written.
func hardenInstallStateFile(dataDir string) {
	os.Chmod(filepath.Join(dataDir, "install-state.json"), 0600)
}

// yesmemApplied reports whether settings.json still carries yesmem's own
// modifications. Either marker means the current values are not a pre-install
// state and must not be recorded as one.
func yesmemApplied(settings map[string]any) bool {
	if env, ok := settings["env"].(map[string]any); ok {
		if baseURL, ok := env["ANTHROPIC_BASE_URL"].(string); ok && baseURL == claudeProxyBaseURL {
			return true
		}
	}
	mcpServers, ok := settings["mcpServers"].(map[string]any)
	if !ok {
		return false
	}
	_, hasYesmem := mcpServers["yesmem"]
	return hasYesmem
}

// loadPreInstallState reads the saved pre-install state.
func loadPreInstallState(dataDir string) map[string]any {
	data, err := os.ReadFile(filepath.Join(dataDir, "install-state.json"))
	if err != nil {
		return nil
	}
	var state map[string]any
	if json.Unmarshal(data, &state) != nil {
		return nil
	}
	return state
}

// setCleanupPeriod sets cleanupPeriodDays in settings.json.
func setCleanupPeriod(settings map[string]any) {
	settings["cleanupPeriodDays"] = 99999
}

// disableAutoCompact disables Claude Code's built-in context compaction.
// Sawtooth cache optimization handles context management via the proxy instead.
func disableAutoCompact(settings map[string]any) {
	settings["autoCompactEnabled"] = false
}

// setProxyEnvVar sets ANTHROPIC_BASE_URL in the settings.json env block.
// This is the official, documented way for Claude Code to read environment variables —
// shell profiles (.bashrc) have an interactive guard and are not sourced by non-interactive contexts.
func setProxyEnvVar(settings map[string]any) {
	env, ok := settings["env"].(map[string]any)
	if !ok {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = claudeProxyBaseURL
	env["CLAUDE_CODE_REPL"] = "true"
	settings["env"] = env
}

// removeProxyEnvVar replaces the yesmem proxy URL in ANTHROPIC_BASE_URL.
// For API-key users (no subscription), we set it to the real API endpoint
// to bypass Claude Code's bridge which requires a subscription.
// For non-API-key users, we remove it entirely.
func removeProxyEnvVar(settings map[string]any) {
	env, ok := settings["env"].(map[string]any)
	if !ok {
		return
	}
	// setProxyEnvVar turned the REPL tooling on alongside the proxy; it is ours
	// and goes away with it.
	delete(env, "CLAUDE_CODE_REPL")
	// If user has an API key, point to real API to bypass bridge
	if _, hasKey := env["ANTHROPIC_API_KEY"]; hasKey {
		env["ANTHROPIC_BASE_URL"] = "https://api.anthropic.com"
	} else {
		delete(env, "ANTHROPIC_BASE_URL")
		if len(env) == 0 {
			delete(settings, "env")
		}
	}
}

// registerMCPPermissions adds the yesmem MCP wildcard to permissions.allow.
func registerMCPPermissions(settings map[string]any) {
	wildcard := "mcp__yesmem__*"

	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		perms = map[string]any{}
	}

	allow, ok := perms["allow"].([]any)
	if !ok {
		allow = []any{}
	}

	for _, v := range allow {
		if s, ok := v.(string); ok && s == wildcard {
			return // already present
		}
	}

	allow = append(allow, wildcard)
	perms["allow"] = allow
	settings["permissions"] = perms
}

// registerMCPServer adds yesmem to mcpServers in settings.json.
func registerMCPInSettings(settings map[string]any, binaryPath string) {
	mcpServers, ok := settings["mcpServers"].(map[string]any)
	if !ok {
		mcpServers = map[string]any{}
	}
	mcpServers["yesmem"] = map[string]any{
		"command": binaryPath,
		"args":    []string{"mcp"},
	}
	settings["mcpServers"] = mcpServers
}

// clearClaudeJSONAuth removes primaryApiKey and oauthAccount from ~/.claude.json
// so that Claude Code uses ANTHROPIC_API_KEY from settings.json env without conflict.
// Skips silently if the file doesn't exist (fresh install without prior Claude Code usage).
func clearClaudeJSONAuth(home string) error {
	path := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse claude.json: %w", err)
	}

	delete(cfg, "primaryApiKey")
	delete(cfg, "oauthAccount")

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// The file predates yesmem on most installs, so the mode literal alone would
	// keep whatever an earlier Claude Code version left behind.
	return writeSecretFile(path, out)
}

// registerMCPInClaudeJSON adds yesmem to ~/.claude.json (user-scope, all projects).
func registerMCPInClaudeJSON(home, binaryPath string) error {
	path := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No claude.json yet — skip
		}
		return err
	}

	// Backup before modifying (only if no backup exists yet)
	backupPath := path + ".bak"
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		if err := writeSecretFile(backupPath, data); err != nil {
			return fmt.Errorf("backup claude.json: %w", err)
		}
	} else if err := os.Chmod(backupPath, 0600); err != nil {
		// The backup holds the pre-install primaryApiKey. A legacy file keeps the
		// mode it was created with, and a failing chmod must not stop the install.
		fmt.Fprintf(os.Stderr, "  warning: could not tighten %s: %v\n", backupPath, err)
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parse claude.json: %w", err)
	}

	mcpServers, ok := config["mcpServers"].(map[string]any)
	if !ok {
		mcpServers = map[string]any{}
	}

	mcpServers["yesmem"] = map[string]any{
		"type":    "stdio",
		"command": binaryPath,
		"args":    []string{"mcp"},
		"env":     map[string]any{},
	}
	config["mcpServers"] = mcpServers

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	// The file predates yesmem on most installs, so the mode literal alone would
	// keep whatever an earlier Claude Code version left behind.
	return writeSecretFile(path, out)
}

// registerHooks adds all yesmem hooks (SessionStart briefing, PreToolUse, PostToolUse, etc.).
// Preserves existing hooks — appends yesmem commands without overwriting.
func registerHooks(settings map[string]any, binaryPath string) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
	}

	// SessionStart hook — briefing injection
	addSessionStartHook(hooks, binaryPath)

	// Think hook removed — reminder injection now handled by the proxy (think.go).
	// Clean up any existing hook-think entries from previous setups.
	removeThinkHook(hooks, binaryPath)

	// UserPromptSubmit — dynamic yesmem-usage reminder
	addIdleTickHook(hooks, binaryPath)

	// SessionEnd — track session ends for recovery + pin cleanup
	addSessionEndHook(hooks, binaryPath)

	// PreToolUse — warn about known gotchas before Bash execution
	addPreToolUseHook(hooks, binaryPath)
	// PreToolUse — evaluate tool calls against RULES.md via LLM
	addPreToolUseGuardHook(hooks, binaryPath)

	// PostToolUseFailure — unified hook (learn + assist combined)
	addPostToolUseFailureCombinedHook(hooks, binaryPath)

	// PostToolUse — auto-resolve unfinished items on git commit
	addPostToolUseResolveHook(hooks, binaryPath)

	settings["hooks"] = hooks
}

func addSessionStartHook(hooks map[string]any, binaryPath string) {
	existing, ok := hooks["SessionStart"].([]any)
	if !ok {
		existing = []any{}
	}

	// Check if yesmem hook already registered — fix matcher if missing
	for i, entry := range existing {
		if m, ok := entry.(map[string]any); ok {
			for _, h := range toHookSlice(m["hooks"]) {
				// The verb matters here: SessionStart also carries our
				// codemap-hook, which is a different handler and must not be
				// rewritten into a briefing hook.
				if cmd, ok := h["command"].(string); ok && (isYesmemHandler(cmd, "briefing-hook", binaryPath) || isLegacyBriefingScript(cmd)) {
					// Ensure matcher is set (fix for hooks registered without matcher)
					if _, hasMatcher := m["matcher"]; !hasMatcher {
						m["matcher"] = "startup|resume|clear|compact"
						existing[i] = m
						hooks["SessionStart"] = existing
					}
					// Update command path if binary moved
					h["command"] = retargetYesmemInvocation(cmd, "briefing-hook", binaryPath)
					return
				}
			}
		}
	}

	// Add new SessionStart hook entry
	existing = append(existing, map[string]any{
		"matcher": "startup|resume|clear|compact",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": binaryPath + " briefing-hook",
			},
		},
	})
	hooks["SessionStart"] = existing
}

// invokesYesmem reports whether one ";"-separated command segment runs the
// yesmem binary. Identity is the binary, not the word "yesmem" somewhere in the
// command: a substring match claims a foreign my-yesmem-notes.sh and rewrites or
// deletes it, while matching a subcommand alone claims a foreign
// idle-tick-notify.sh. Recognized are binaryPath itself — as the whole segment or
// as its prefix, which also covers a path containing spaces — the basename of
// binaryPath (the same binary under a path it has since left) and a yesmem-named
// program, which covers the legacy yesmem-briefing.sh script.
func invokesYesmem(seg, binaryPath string) bool {
	if binaryPath != "" && (seg == binaryPath || strings.HasPrefix(seg, binaryPath+" ")) {
		return true
	}
	fields := strings.Fields(seg)
	if len(fields) == 0 {
		return false
	}
	base := filepath.Base(fields[0])
	if strings.HasPrefix(base, "yesmem") {
		return true
	}
	return binaryPath != "" && base == filepath.Base(binaryPath)
}

// segNamesVerb reports whether a segment names the subcommand as one of its own
// words, so that a foreign hook-check-wrapper.sh is not taken for our hook-check.
func segNamesVerb(seg, verb string) bool {
	for _, f := range strings.Fields(seg) {
		if f == verb {
			return true
		}
	}
	return false
}

// isYesmemHandler reports whether cmd is one of our hook handlers. verb names the
// subcommand the caller looks for; it separates our handlers from each other
// (PreToolUse carries hook-check and hook-guard side by side) and from another of
// our handlers such as the legacy micro-reminder. An empty verb accepts any
// invocation of the binary.
func isYesmemHandler(cmd, verb, binaryPath string) bool {
	for _, seg := range strings.Split(cmd, ";") {
		if !invokesYesmem(seg, binaryPath) {
			continue
		}
		if verb == "" || segNamesVerb(seg, verb) {
			return true
		}
	}
	return false
}

// isLegacyBriefingScript reports whether cmd is the shell wrapper yesmem
// registered before `briefing-hook` existed. It carries no subcommand, so the
// verb test alone would leave it behind instead of migrating it.
func isLegacyBriefingScript(cmd string) bool {
	for _, seg := range strings.Split(cmd, ";") {
		fields := strings.Fields(seg)
		if len(fields) != 1 {
			continue
		}
		if strings.HasPrefix(filepath.Base(fields[0]), "yesmem-briefing") {
			return true
		}
	}
	return false
}

// retargetYesmemInvocation points our invocation of verb at binaryPath. Only the
// binary changes: the arguments behind the verb survive, and so does every other
// segment of a chained command — an older version appended idle-tick to an
// existing hook, so a chain is a real shape in settings.json. The legacy briefing
// script carries no subcommand and is replaced as a whole.
func retargetYesmemInvocation(cmd, verb, binaryPath string) string {
	target := binaryPath + " " + verb
	segs := strings.Split(cmd, ";")
	for i, seg := range segs {
		if off := yesmemVerbOffset(seg, verb, binaryPath); off >= 0 {
			segs[i] = target + seg[off+len(verb):]
			continue
		}
		if verb == "briefing-hook" && isLegacyBriefingScript(seg) {
			segs[i] = target
		}
	}
	return strings.Join(segs, ";")
}

// yesmemVerbOffset returns the offset of the verb token that ends our binary part,
// or -1. Only a verb behind a prefix that is our binary counts: a path component
// that happens to spell the verb must not be taken for the subcommand, or every
// run would prepend another copy of the binary.
func yesmemVerbOffset(seg, verb, binaryPath string) int {
	offset := 0
	for _, tok := range strings.Fields(seg) {
		i := strings.Index(seg[offset:], tok)
		if i < 0 {
			return -1
		}
		offset += i
		if tok == verb && invokesYesmem(seg[:offset], binaryPath) {
			return offset
		}
		offset += len(tok)
	}
	return -1
}

// stripCommandSegment removes every ";"-separated segment that is our invocation
// of needle, returning the remaining chain. Matching the segment instead of a
// binaryPath+command pair also covers installs whose binary path has moved.
// Unchanged chains are returned byte-identical, and the kept segments keep their
// original text, so a foreign command is never reformatted — not even a quoted
// ";" that the split above had to cut through.
func stripCommandSegment(cmd, needle, binaryPath string) string {
	var kept []string
	changed := false
	for _, seg := range strings.Split(cmd, ";") {
		if isYesmemHandler(seg, needle, binaryPath) {
			changed = true
			continue
		}
		if strings.TrimSpace(seg) == "" {
			continue
		}
		kept = append(kept, seg)
	}
	if !changed {
		return cmd
	}
	return strings.Join(kept, ";")
}

// allHandlersAreYesmem reports whether every handler in a matcher group is a
// yesmem hook, which is what makes it safe to rewrite group-level fields.
func allHandlersAreYesmem(m map[string]any, binaryPath string) bool {
	handlers := toHookSlice(m["hooks"])
	if len(handlers) == 0 {
		return false
	}
	for _, h := range handlers {
		if cmd, _ := h["command"].(string); !isYesmemHandler(cmd, "", binaryPath) {
			return false
		}
	}
	return true
}

func addIdleTickHook(hooks map[string]any, binaryPath string) {
	idleCmd := binaryPath + " idle-tick"
	oldCmd := "micro-reminder"

	existing, ok := hooks["UserPromptSubmit"].([]any)
	if !ok {
		existing = []any{}
	}

	registered := false
	for _, entry := range existing {
		if m, ok := entry.(map[string]any); ok {
			for _, h := range toHookSlice(m["hooks"]) {
				if cmd, ok := h["command"].(string); ok && isYesmemHandler(cmd, "idle-tick", binaryPath) {
					registered = true
					// Update command path if binary moved — otherwise the
					// registration keeps pointing at a path that is gone.
					h["command"] = retargetYesmemInvocation(cmd, "idle-tick", binaryPath)
				}
			}
		}
	}

	// Drop legacy micro-reminder segments, keep every other handler untouched.
	// This runs even when idle-tick is already registered, so a stale legacy
	// entry cannot survive just because the migration already ran once.
	kept := make([]any, 0, len(existing)+1)
	for _, entry := range existing {
		m, ok := entry.(map[string]any)
		if !ok {
			kept = append(kept, entry)
			continue
		}
		var handlers []map[string]any
		for _, h := range toHookSlice(m["hooks"]) {
			if cmd, ok := h["command"].(string); ok {
				if rest := stripCommandSegment(cmd, oldCmd, binaryPath); rest != cmd {
					if rest == "" {
						continue // handler existed only for micro-reminder
					}
					h["command"] = rest
				}
			}
			handlers = append(handlers, h)
		}
		if len(handlers) == 0 {
			continue // entry is now empty
		}
		m["hooks"] = toAnySlice(handlers)
		kept = append(kept, m)
	}

	// Register as its own entry — splicing into a foreign command chain with "; "
	// would make idle-tick's exit code mask a user hook that blocked the prompt.
	if !registered {
		kept = append(kept, map[string]any{
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": idleCmd,
				},
			},
		})
	}
	hooks["UserPromptSubmit"] = kept
}

// sessionEndTimeout is the per-handler timeout for `yesmem session-end`. Claude
// Code gives SessionEnd hooks a shared 1.5s budget by default, but the hook dials
// the daemon with a 3s connect and a 3s call deadline, so without a raised budget
// a busy daemon makes the tracking fail silently.
const sessionEndTimeout = 10

// addSessionEndHook registers session-end tracking. Deliberately without a
// matcher: every reason Claude Code can report reaches the hook and RunSessionEnd
// selects the ones yesmem tracks. The documented SessionEnd reasons are clear,
// resume, logout, prompt_input_exit and other — "compact" is not one of them, so
// it must not be baked into a matcher.
func addSessionEndHook(hooks map[string]any, binaryPath string) {
	hookCmd := binaryPath + " session-end"
	existing, ok := hooks["SessionEnd"].([]any)
	if !ok {
		existing = []any{}
	}

	for i, entry := range existing {
		if m, ok := entry.(map[string]any); ok {
			for _, h := range toHookSlice(m["hooks"]) {
				// Our own canonical command, or a yesmem hook recorded under a
				// different binary path. The subcommand alone is not enough: it
				// also matches a foreign session-end-notify.sh.
				cmd, _ := h["command"].(string)
				if cmd != hookCmd && !isYesmemHandler(cmd, "session-end", binaryPath) {
					continue
				}
				// Update command path if binary moved
				h["command"] = retargetYesmemInvocation(cmd, "session-end", binaryPath)
				h["timeout"] = sessionEndTimeout
				// The hook fires for every reason and filters internally, so a
				// matcher would only narrow it away — but dropping it is only safe
				// when the whole group is ours, or a foreign handler sharing the
				// group silently widens.
				if allHandlersAreYesmem(m, binaryPath) {
					delete(m, "matcher")
				}
				existing[i] = m
				hooks["SessionEnd"] = existing
				return
			}
		}
	}

	existing = append(existing, map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCmd,
				"timeout": sessionEndTimeout,
			},
		},
	})
	hooks["SessionEnd"] = existing
}

// removeThinkHook removes the deprecated hook-think entry from UserPromptSubmit.
// Think reminder injection is now handled by the proxy (think.go).
func removeThinkHook(hooks map[string]any, binaryPath string) {
	existing, ok := hooks["UserPromptSubmit"].([]any)
	if !ok {
		return
	}

	var cleaned []any
	for _, entry := range existing {
		m, ok := entry.(map[string]any)
		if !ok {
			cleaned = append(cleaned, entry)
			continue
		}
		isThink := false
		for _, h := range toHookSlice(m["hooks"]) {
			if cmd, ok := h["command"].(string); ok {
				if isYesmemHandler(cmd, "hook-think", binaryPath) {
					isThink = true
					break
				}
			}
		}
		if !isThink {
			cleaned = append(cleaned, entry)
		}
	}
	hooks["UserPromptSubmit"] = cleaned
}

func addPreToolUseHook(hooks map[string]any, binaryPath string) {
	hookCmd := binaryPath + " hook-check"
	existing, ok := hooks["PreToolUse"].([]any)
	if !ok {
		existing = []any{}
	}

	for i, entry := range existing {
		if m, ok := entry.(map[string]any); ok {
			for _, h := range toHookSlice(m["hooks"]) {
				// Match on our binary plus the subcommand, not on "yesmem":
				// every command containing the word also matches a foreign
				// my-yesmem-notes.sh, and hook-guard shares the binary.
				if cmd, ok := h["command"].(string); ok && isYesmemHandler(cmd, "hook-check", binaryPath) {
					m["matcher"] = ".*"
					h["command"] = retargetYesmemInvocation(cmd, "hook-check", binaryPath) // update path if the binary moved
					existing[i] = m
					hooks["PreToolUse"] = existing
					return
				}
			}
		}
	}

	existing = append(existing, map[string]any{
		"matcher": ".*",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCmd,
			},
		},
	})
	hooks["PreToolUse"] = existing
}

// hookGuardMatcher is the canonical PreToolUse matcher for `yesmem hook-guard`.
// Bumping this string upgrades existing installations on the next `yesmem update`.
const hookGuardMatcher = "Bash|REPL|Edit|Write"

// hookResolveIf narrows `yesmem hook-resolve` to git commands. It sits on the
// hook handler: Claude Code reads `if` from the individual handler, not from the
// enclosing matcher group.
const hookResolveIf = "Bash(git *)"

func addPreToolUseGuardHook(hooks map[string]any, binaryPath string) {
	hookCmd := binaryPath + " hook-guard"
	existing, ok := hooks["PreToolUse"].([]any)
	if !ok {
		existing = []any{}
	}

	// Check if already present — upgrade matcher in-place if outdated
	for _, entry := range existing {
		if m, ok := entry.(map[string]any); ok {
			for _, h := range toHookSlice(m["hooks"]) {
				if cmd, ok := h["command"].(string); ok && isYesmemHandler(cmd, "hook-guard", binaryPath) {
					if m["matcher"] != hookGuardMatcher {
						m["matcher"] = hookGuardMatcher
					}
					h["command"] = retargetYesmemInvocation(cmd, "hook-guard", binaryPath) // update path if the binary moved
					return
				}
			}
		}
	}

	// Add as second PreToolUse entry (runs after hook-check)
	existing = append(existing, map[string]any{
		"matcher": hookGuardMatcher,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCmd,
			},
		},
	})
	hooks["PreToolUse"] = existing
}

// addPostToolUseFailureCombinedHook registers the unified hook-failure command,
// migrating from the old hook-learn + hook-assist pair.
func addPostToolUseFailureCombinedHook(hooks map[string]any, binaryPath string) {
	hookCmd := binaryPath + " hook-failure"
	existing, ok := hooks["PostToolUseFailure"].([]any)
	if !ok {
		existing = []any{}
	}

	// Check if already registered — update matcher if needed
	for i, entry := range existing {
		if m, ok := entry.(map[string]any); ok {
			for _, h := range toHookSlice(m["hooks"]) {
				if cmd, ok := h["command"].(string); ok && isYesmemHandler(cmd, "hook-failure", binaryPath) {
					m["matcher"] = ".*"
					h["command"] = retargetYesmemInvocation(cmd, "hook-failure", binaryPath) // update path if the binary moved
					existing[i] = m
					hooks["PostToolUseFailure"] = existing
					return
				}
			}
		}
	}

	// Remove old hook-learn and hook-assist entries, replace with hook-failure
	for _, entry := range existing {
		if m, ok := entry.(map[string]any); ok {
			matcher, _ := m["matcher"].(string)
			if matcher == "Bash" || matcher == "Bash|WebFetch|WebSearch" || matcher == "" {
				// Filter out old hooks, add new one
				var kept []map[string]any
				for _, h := range toHookSlice(m["hooks"]) {
					if cmd, ok := h["command"].(string); ok {
						if isYesmemHandler(cmd, "hook-learn", binaryPath) || isYesmemHandler(cmd, "hook-assist", binaryPath) {
							continue // Remove old
						}
					}
					kept = append(kept, h)
				}
				kept = append(kept, map[string]any{
					"type":    "command",
					"command": hookCmd,
				})
				m["hooks"] = toAnySlice(kept)
				hooks["PostToolUseFailure"] = existing
				return
			}
		}
	}

	// Fallback: add as new entry
	existing = append(existing, map[string]any{
		"matcher": ".*",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCmd,
			},
		},
	})
	hooks["PostToolUseFailure"] = existing
}

func addPostToolUseResolveHook(hooks map[string]any, binaryPath string) {
	hookCmd := binaryPath + " hook-resolve"
	existing, ok := hooks["PostToolUse"].([]any)
	if !ok {
		existing = []any{}
	}

	// Check if already registered
	for _, entry := range existing {
		if m, ok := entry.(map[string]any); ok {
			for _, h := range toHookSlice(m["hooks"]) {
				if cmd, ok := h["command"].(string); ok && isYesmemHandler(cmd, "hook-resolve", binaryPath) {
					// Migrate installations written back when `if` sat on the
					// matcher group, where Claude Code never reads it.
					delete(m, "if")
					h["if"] = hookResolveIf
					h["command"] = retargetYesmemInvocation(cmd, "hook-resolve", binaryPath) // update path if the binary moved
					return
				}
			}
		}
	}

	existing = append(existing, map[string]any{
		"matcher": "Bash",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCmd,
				"if":      hookResolveIf,
			},
		},
	})
	hooks["PostToolUse"] = existing
}

func toHookSlice(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var result []map[string]any
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			result = append(result, m)
		}
	}
	return result
}

// EnsureHooks reads settings.json, registers/updates all yesmem hooks, and writes back.
// Safe to call repeatedly — preserves existing hooks, only adds/updates yesmem entries.
func EnsureHooks() error {
	home, _ := os.UserHomeDir()
	binaryPath, _ := os.Executable()

	// Installations that predate the 0600 fix keep a world-readable state file;
	// `yesmem update` is the path that reaches them.
	hardenInstallStateFile(filepath.Join(home, ".claude", "yesmem"))

	settings, err := readSettingsJSON(home)
	if err != nil {
		return fmt.Errorf("read settings.json: %w", err)
	}

	registerHooks(settings, binaryPath)
	registerMCPPermissions(settings)
	registerStatusLine(settings, binaryPath)

	if err := writeSettingsJSON(home, settings); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}
	return nil
}

// registerStatusLine sets the custom statusline command in settings.json.
func registerStatusLine(settings map[string]any, binaryPath string) {
	settings["statusLine"] = map[string]any{
		"type":            "command",
		"command":         binaryPath + " statusline",
		"refreshInterval": 2,
	}
}

func toAnySlice(hooks []map[string]any) []any {
	result := make([]any, len(hooks))
	for i, h := range hooks {
		result[i] = h
	}
	return result
}

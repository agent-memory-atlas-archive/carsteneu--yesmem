package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hookCommands returns every handler command registered for a hook event.
func hookCommands(hooks map[string]any, event string) []string {
	var cmds []string
	entries, _ := hooks[event].([]any)
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		for _, h := range toHookSlice(m["hooks"]) {
			if cmd, ok := h["command"].(string); ok {
				cmds = append(cmds, cmd)
			}
		}
	}
	return cmds
}

func countContaining(cmds []string, sub string) int {
	n := 0
	for _, c := range cmds {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func TestAddPreToolUseGuardHook_UpgradesOutdatedMatcher(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": "Bash|Edit|Write",
				"hooks": []any{
					map[string]any{
						"type":    "command",
						"command": "/usr/local/bin/yesmem hook-guard",
					},
				},
			},
		},
	}

	addPreToolUseGuardHook(hooks, "/usr/local/bin/yesmem")

	entries, ok := hooks["PreToolUse"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("expected single PreToolUse entry, got %v", hooks["PreToolUse"])
	}
	entry := entries[0].(map[string]any)
	if entry["matcher"] != hookGuardMatcher {
		t.Errorf("expected matcher %q, got %q", hookGuardMatcher, entry["matcher"])
	}
}

func TestRegisterMCPPermissions_Empty(t *testing.T) {
	settings := map[string]any{}
	registerMCPPermissions(settings)

	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatal("permissions not set")
	}
	allow, ok := perms["allow"].([]any)
	if !ok {
		t.Fatal("permissions.allow not set")
	}
	if len(allow) != 1 {
		t.Fatalf("expected 1 entry (mcp__yesmem__*), got %d: %v", len(allow), allow)
	}
	if allow[0] != "mcp__yesmem__*" {
		t.Fatalf("expected mcp__yesmem__*, got %q", allow[0])
	}
}

func TestRegisterMCPPermissions_ExistingPermissions(t *testing.T) {
	settings := map[string]any{
		"permissions": map[string]any{
			"allow": []any{"Bash(git:*)", "WebSearch"},
		},
	}
	registerMCPPermissions(settings)

	perms := settings["permissions"].(map[string]any)
	allow := perms["allow"].([]any)
	if len(allow) != 3 {
		t.Fatalf("expected 3 entries (2 existing + mcp__yesmem__*), got %d: %v", len(allow), allow)
	}
	if allow[0] != "Bash(git:*)" || allow[1] != "WebSearch" {
		t.Fatalf("existing entries should be preserved at start: %v", allow[:2])
	}
	if allow[2] != "mcp__yesmem__*" {
		t.Fatalf("expected mcp__yesmem__* as third entry, got %q", allow[2])
	}
}

func TestRegisterMCPPermissions_AlreadyPresent(t *testing.T) {
	settings := map[string]any{
		"permissions": map[string]any{
			"allow": []any{"Bash(git:*)", "mcp__yesmem__*"},
		},
	}
	registerMCPPermissions(settings)

	allow := settings["permissions"].(map[string]any)["allow"].([]any)
	count := 0
	for _, v := range allow {
		if v == "mcp__yesmem__*" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 mcp__yesmem__*, got %d in %v", count, allow)
	}
}

func TestAddPostToolUseResolveHook_HasIfCondition(t *testing.T) {
	hooks := map[string]any{}
	addPostToolUseResolveHook(hooks, "/usr/bin/yesmem")

	entries, ok := hooks["PostToolUse"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("expected 1 PostToolUse entry, got %v", hooks["PostToolUse"])
	}
	entry := entries[0].(map[string]any)
	if _, ok := entry["if"]; ok {
		t.Fatal("if must not sit on the matcher group — Claude Code reads it from the hook handler")
	}
	handler := toHookSlice(entry["hooks"])[0]
	ifVal, ok := handler["if"].(string)
	if !ok {
		t.Fatal("if field not set on the hook-resolve handler")
	}
	if ifVal != "Bash(git *)" {
		t.Fatalf("expected 'Bash(git *)', got %q", ifVal)
	}
}

func TestAddPostToolUseResolveHook_MigratesExistingWithoutIf(t *testing.T) {
	hooks := map[string]any{
		"PostToolUse": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{
						"type":    "command",
						"command": "/usr/bin/yesmem hook-resolve",
					},
				},
			},
		},
	}
	addPostToolUseResolveHook(hooks, "/usr/bin/yesmem")

	entries := hooks["PostToolUse"].([]any)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (no duplicate), got %d", len(entries))
	}
	entry := entries[0].(map[string]any)
	handler := toHookSlice(entry["hooks"])[0]
	ifVal, ok := handler["if"].(string)
	if !ok {
		t.Fatal("if field not migrated onto the existing hook handler")
	}
	if ifVal != "Bash(git *)" {
		t.Fatalf("expected 'Bash(git *)', got %q", ifVal)
	}
}

func TestAddPostToolUseResolveHook_MovesGroupLevelIfToHandler(t *testing.T) {
	hooks := map[string]any{
		"PostToolUse": []any{
			map[string]any{
				"matcher": "Bash",
				"if":      "Bash(git *)",
				"hooks": []any{
					map[string]any{
						"type":    "command",
						"command": "/usr/bin/yesmem hook-resolve",
					},
				},
			},
		},
	}
	addPostToolUseResolveHook(hooks, "/usr/bin/yesmem")

	entries := hooks["PostToolUse"].([]any)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (no duplicate), got %d", len(entries))
	}
	entry := entries[0].(map[string]any)
	if _, ok := entry["if"]; ok {
		t.Fatal("group-level if was not cleaned up during migration")
	}
	handler := toHookSlice(entry["hooks"])[0]
	if handler["if"] != "Bash(git *)" {
		t.Fatalf("handler if = %v, want 'Bash(git *)'", handler["if"])
	}
}

func TestRegisterMCPPermissions_PermissionsWithoutAllow(t *testing.T) {
	settings := map[string]any{
		"permissions": map[string]any{
			"defaultMode": "default",
		},
	}
	registerMCPPermissions(settings)

	perms := settings["permissions"].(map[string]any)
	allow, ok := perms["allow"].([]any)
	if !ok {
		t.Fatal("permissions.allow not created")
	}
	if len(allow) != 1 {
		t.Fatalf("expected 1 entry (mcp__yesmem__*), got %d: %v", len(allow), allow)
	}
	if allow[0] != "mcp__yesmem__*" {
		t.Fatalf("expected mcp__yesmem__*, got %q", allow[0])
	}
	// defaultMode should be preserved
	if perms["defaultMode"] != "default" {
		t.Fatal("defaultMode was overwritten")
	}
}

func TestAddSessionStartHook_UsesBriefingHookCommand(t *testing.T) {
	hooks := map[string]any{}
	addSessionStartHook(hooks, "/usr/local/bin/yesmem")

	entries, ok := hooks["SessionStart"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatal("SessionStart hook not registered")
	}
	hookList := entries[0].(map[string]any)["hooks"].([]any)
	cmd := hookList[0].(map[string]any)["command"].(string)
	want := "/usr/local/bin/yesmem briefing-hook"
	if cmd != want {
		t.Errorf("hook command = %q want %q", cmd, want)
	}
}

func TestAddSessionStartHook_UpdatesExistingCommandToBriefingHook(t *testing.T) {
	hooks := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "startup|resume|clear|compact",
				"hooks": []any{
					map[string]any{"type": "command", "command": "/home/user/.claude/hooks/yesmem-briefing.sh"},
				},
			},
		},
	}
	addSessionStartHook(hooks, "/usr/local/bin/yesmem")

	entries := hooks["SessionStart"].([]any)
	hookList := entries[0].(map[string]any)["hooks"].([]any)
	cmd := hookList[0].(map[string]any)["command"].(string)
	want := "/usr/local/bin/yesmem briefing-hook"
	if cmd != want {
		t.Errorf("existing hook not updated: got %q want %q", cmd, want)
	}
}

func TestSetPrimaryApiKeyInClaudeJSON_RemovesOAuthAccount(t *testing.T) {
	home := t.TempDir()
	existing := `{"numStartups":5,"primaryApiKey":"sk-old","oauthAccount":{"emailAddress":"test@test.com","displayName":"Test"}}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(existing), 0600)

	err := clearClaudeJSONAuth(home)
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	if _, ok := cfg["primaryApiKey"]; ok {
		t.Fatal("primaryApiKey should be removed")
	}
	if _, ok := cfg["oauthAccount"]; ok {
		t.Fatal("oauthAccount should be removed")
	}
	if cfg["numStartups"] != float64(5) {
		t.Fatal("other fields should be preserved")
	}
}

// clearClaudeJSONAuth rewrites a file whose pre-yesmem version carried no strict
// mode: the chmod must not depend on the file being created by this call.
func TestClearClaudeJSONAuth_TightensExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"primaryApiKey":"sk-old"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := clearClaudeJSONAuth(home); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf(".claude.json mode = %04o, want 0600 — the file carries OAuth state", got)
	}
}

func TestRegisterMCPInClaudeJSON_TightensExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"numStartups":5}`), 0664); err != nil {
		t.Fatal(err)
	}

	if err := registerMCPInClaudeJSON(home, "/usr/local/bin/yesmem"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf(".claude.json mode = %04o, want 0600 — the file carries primaryApiKey on real installs", got)
	}
}

// A backup an older version left behind keeps its mode; it carries the
// pre-install primaryApiKey even though this call does not rewrite it.
func TestRegisterMCPInClaudeJSON_TightensExistingBackup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"numStartups":5}`), 0600); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(home, ".claude.json.bak")
	if err := os.WriteFile(backupPath, []byte(`{"primaryApiKey":"sk-old"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := registerMCPInClaudeJSON(home, "/usr/local/bin/yesmem"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf(".claude.json.bak mode = %04o, want 0600 — the backup holds the pre-install key", got)
	}
}

func TestSavePreInstallState_SavesOAuthAccount(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	existing := `{"primaryApiKey":"sk-old","oauthAccount":{"emailAddress":"test@test.com"}}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(existing), 0600)

	settings := map[string]any{}
	savePreInstallState(dataDir, settings, home)

	state := loadPreInstallState(dataDir)
	if state == nil {
		t.Fatal("state not saved")
	}
	oa, ok := state["oauthAccount"].(map[string]any)
	if !ok {
		t.Fatal("oauthAccount not saved in pre-install state")
	}
	if oa["emailAddress"] != "test@test.com" {
		t.Fatalf("expected test@test.com, got %v", oa["emailAddress"])
	}
}

func TestClearClaudeJSONAuth_RemovesBothKeys(t *testing.T) {
	home := t.TempDir()
	existing := `{"numStartups":5,"primaryApiKey":"sk-old-key","mcpServers":{}}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(existing), 0600)

	err := clearClaudeJSONAuth(home)
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	if _, ok := cfg["primaryApiKey"]; ok {
		t.Fatal("primaryApiKey should be removed")
	}
	if cfg["numStartups"] != float64(5) {
		t.Fatal("other fields should be preserved")
	}
}

func TestClearClaudeJSONAuth_NoFileNoError(t *testing.T) {
	home := t.TempDir()
	err := clearClaudeJSONAuth(home)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
}

func TestRegisterStatusLine_SetsRefreshInterval(t *testing.T) {
	settings := map[string]any{}
	registerStatusLine(settings, "/usr/bin/yesmem")

	sl, ok := settings["statusLine"].(map[string]any)
	if !ok {
		t.Fatal("statusLine not set")
	}
	if sl["type"] != "command" {
		t.Fatalf("type = %v, want command", sl["type"])
	}
	if sl["command"] != "/usr/bin/yesmem statusline" {
		t.Fatalf("command = %v, want '/usr/bin/yesmem statusline'", sl["command"])
	}
	ri, ok := sl["refreshInterval"].(int)
	if !ok {
		t.Fatal("refreshInterval not set")
	}
	if ri != 2 {
		t.Fatalf("refreshInterval = %d, want 2", ri)
	}
}

func TestSavePreInstallState_SavesAPIKey(t *testing.T) {
	dir := t.TempDir()
	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY": "sk-ant-existing",
		},
	}
	savePreInstallState(dir, settings, "")

	state := loadPreInstallState(dir)
	if state == nil {
		t.Fatal("state not saved")
	}
	if state["envAPIKey"] != "sk-ant-existing" {
		t.Fatalf("expected sk-ant-existing, got %v", state["envAPIKey"])
	}
}

func TestSavePreInstallState_SavesAbsentAPIKey(t *testing.T) {
	dir := t.TempDir()
	settings := map[string]any{}
	savePreInstallState(dir, settings, "")

	state := loadPreInstallState(dir)
	if state == nil {
		t.Fatal("state not saved")
	}
	if state["envAPIKey"] != nil {
		t.Fatalf("absent key should be nil, got %v", state["envAPIKey"])
	}
}

func TestSavePreInstallState_FileModeIs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	dataDir := t.TempDir()
	savePreInstallState(dataDir, map[string]any{}, "")

	info, err := os.Stat(filepath.Join(dataDir, "install-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("install-state.json mode = %04o, want 0600 — the file carries primaryApiKey", got)
	}
}

func TestSavePreInstallState_KeepsOriginalStateWhenYesmemAlreadyApplied(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()

	// First run captures the genuine pre-install state.
	savePreInstallState(dataDir, map[string]any{
		"autoCompactEnabled": true,
		"env":                map[string]any{"ANTHROPIC_API_KEY": "sk-original"},
	}, home)

	// Setup runs again: settings.json now carries yesmem's own modifications.
	savePreInstallState(dataDir, map[string]any{
		"autoCompactEnabled": false,
		"env": map[string]any{
			"ANTHROPIC_API_KEY":  "sk-installed-by-yesmem",
			"ANTHROPIC_BASE_URL": "http://localhost:9099",
		},
		"mcpServers": map[string]any{
			"yesmem": map[string]any{"command": "/usr/local/bin/yesmem"},
		},
	}, home)

	state := loadPreInstallState(dataDir)
	if state == nil {
		t.Fatal("state not saved")
	}
	if state["autoCompactEnabled"] != true {
		t.Fatalf("autoCompactEnabled = %v, want true — a re-run must not overwrite the genuine pre-install value", state["autoCompactEnabled"])
	}
	if state["envAPIKey"] != "sk-original" {
		t.Fatalf("envAPIKey = %v, want sk-original — a re-run must not overwrite the genuine pre-install key", state["envAPIKey"])
	}
}

func TestRegisterHooks_RegistersSessionEnd(t *testing.T) {
	settings := map[string]any{}
	registerHooks(settings, "/usr/local/bin/yesmem")
	hooks := settings["hooks"].(map[string]any)

	want := "/usr/local/bin/yesmem session-end"
	if n := countContaining(hookCommands(hooks, "SessionEnd"), want); n != 1 {
		t.Fatalf("SessionEnd registrations = %d, want 1 (commands: %v)", n, hookCommands(hooks, "SessionEnd"))
	}
}

func TestRegisterHooks_RegistersIdleTick(t *testing.T) {
	settings := map[string]any{}
	registerHooks(settings, "/usr/local/bin/yesmem")
	hooks := settings["hooks"].(map[string]any)

	if n := countContaining(hookCommands(hooks, "UserPromptSubmit"), "idle-tick"); n != 1 {
		t.Fatalf("idle-tick registrations = %d, want 1 (commands: %v)", n, hookCommands(hooks, "UserPromptSubmit"))
	}
}

func TestRegisterHooks_IsIdempotent(t *testing.T) {
	settings := map[string]any{}
	registerHooks(settings, "/usr/local/bin/yesmem")
	registerHooks(settings, "/usr/local/bin/yesmem")
	hooks := settings["hooks"].(map[string]any)

	for _, want := range []struct{ event, cmd string }{
		{"SessionStart", "briefing-hook"},
		{"SessionEnd", "session-end"},
		{"UserPromptSubmit", "idle-tick"},
		{"PreToolUse", "hook-check"},
		{"PreToolUse", "hook-guard"},
		{"PostToolUse", "hook-resolve"},
		{"PostToolUseFailure", "hook-failure"},
	} {
		n := countContaining(hookCommands(hooks, want.event), want.cmd)
		if n != 1 {
			t.Errorf("%s/%s registered %d times, want exactly 1 (commands: %v)",
				want.event, want.cmd, n, hookCommands(hooks, want.event))
		}
	}
}

func TestAddPreToolUseHook_RegistersWhenOnlyGuardPresent(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": hookGuardMatcher,
				"hooks": []any{
					map[string]any{"type": "command", "command": "/usr/local/bin/yesmem hook-guard"},
				},
			},
		},
	}

	addPreToolUseHook(hooks, "/usr/local/bin/yesmem")

	if n := countContaining(hookCommands(hooks, "PreToolUse"), "hook-check"); n != 1 {
		t.Fatalf("hook-check registrations = %d, want 1 — a hook-guard entry must not count as hook-check (commands: %v)",
			n, hookCommands(hooks, "PreToolUse"))
	}
	if n := countContaining(hookCommands(hooks, "PreToolUse"), "hook-guard"); n != 1 {
		t.Fatalf("hook-guard registrations = %d, want 1", n)
	}

	// The guard entry must keep its own matcher instead of being repointed at ".*".
	for _, entry := range hooks["PreToolUse"].([]any) {
		m := entry.(map[string]any)
		for _, h := range toHookSlice(m["hooks"]) {
			cmd, _ := h["command"].(string)
			if strings.Contains(cmd, "hook-guard") && m["matcher"] != hookGuardMatcher {
				t.Fatalf("hook-guard matcher = %v, want %q", m["matcher"], hookGuardMatcher)
			}
		}
	}
}

func TestAddPreToolUseHook_DoesNotDuplicateHookCheck(t *testing.T) {
	hooks := map[string]any{}
	addPreToolUseHook(hooks, "/usr/local/bin/yesmem")
	addPreToolUseHook(hooks, "/usr/local/bin/yesmem")

	if n := countContaining(hookCommands(hooks, "PreToolUse"), "hook-check"); n != 1 {
		t.Fatalf("hook-check registrations = %d, want 1 (commands: %v)", n, hookCommands(hooks, "PreToolUse"))
	}
}

func TestSavePreInstallState_TightensExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "install-state.json")
	if err := os.WriteFile(path, []byte(`{"envAPIKey":null}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Settings are untouched by yesmem, so the write path runs.
	savePreInstallState(dataDir, map[string]any{}, "")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("mode = %04o, want 0600 — os.WriteFile keeps an existing file's mode", got)
	}
}

func TestSavePreInstallState_TightensExistingFileWhenYesmemApplied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "install-state.json")
	if err := os.WriteFile(path, []byte(`{"envAPIKey":null}`), 0644); err != nil {
		t.Fatal(err)
	}

	// yesmem is installed: the capture is skipped, but the file still has to be
	// tightened — this is the path `yesmem setup` takes on every existing install.
	savePreInstallState(dataDir, map[string]any{
		"env": map[string]any{"ANTHROPIC_BASE_URL": "http://localhost:9099"},
	}, "")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("mode = %04o, want 0600 — the early return must still tighten the file", got)
	}
}

func TestAddSessionEndHook_SetsTimeout(t *testing.T) {
	hooks := map[string]any{}
	addSessionEndHook(hooks, "/usr/local/bin/yesmem")

	entry := hooks["SessionEnd"].([]any)[0].(map[string]any)
	handler := toHookSlice(entry["hooks"])[0]
	timeout, ok := handler["timeout"].(int)
	if !ok {
		t.Fatalf("no timeout on the SessionEnd handler (%v) — the default 1.5s budget is below the hook's 3s dial + 3s call deadline",
			handler["timeout"])
	}
	if timeout < 7 {
		t.Fatalf("timeout = %ds, too low for the hook's 3s dial + 3s call deadline", timeout)
	}
}

func TestAddSessionEndHook_RemovesStaleMatcher(t *testing.T) {
	hooks := map[string]any{
		"SessionEnd": []any{
			map[string]any{
				"matcher": "clear|compact",
				"hooks": []any{
					map[string]any{"type": "command", "command": "/usr/local/bin/yesmem session-end"},
				},
			},
		},
	}
	addSessionEndHook(hooks, "/usr/local/bin/yesmem")

	entries := hooks["SessionEnd"].([]any)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (no duplicate), got %d", len(entries))
	}
	entry := entries[0].(map[string]any)
	if _, ok := entry["matcher"]; ok {
		t.Fatalf("stale matcher %v kept — the hook must fire for every reason and filter internally",
			entry["matcher"])
	}
}

func TestAddSessionEndHook_KeepsForeignHook(t *testing.T) {
	foreign := "/home/user/hooks/session-end-notify.sh"
	hooks := map[string]any{
		"SessionEnd": []any{
			map[string]any{
				"matcher": "clear",
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addSessionEndHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "SessionEnd")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign SessionEnd hook was clobbered: %v", cmds)
	}
	if n := countContaining(cmds, "session-end"); n != 2 {
		t.Fatalf("expected foreign + yesmem hook, got %v", cmds)
	}
}

func TestAddIdleTickHook_MigratesLegacyReminderFromMovedPath(t *testing.T) {
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": "/old/place/yesmem micro-reminder"},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if countContaining(cmds, "micro-reminder") != 0 {
		t.Fatalf("legacy micro-reminder survived a moved binary path: %v", cmds)
	}
	if n := countContaining(cmds, "idle-tick"); n != 1 {
		t.Fatalf("idle-tick registrations = %d, want 1 (commands: %v)", n, cmds)
	}
}

func TestAddIdleTickHook_KeepsForeignHandlerSeparate(t *testing.T) {
	foreign := "/home/user/bin/my-own-hook.sh"
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign handler was rewritten: %v", cmds)
	}
	for _, c := range cmds {
		if strings.Contains(c, ";") {
			t.Fatalf("idle-tick was spliced into a foreign command chain: %q", c)
		}
	}
	if n := countContaining(cmds, "idle-tick"); n != 1 {
		t.Fatalf("idle-tick registrations = %d, want 1 (commands: %v)", n, cmds)
	}
}

func TestAddPostToolUseResolveHook_UpdatesMovedBinaryPath(t *testing.T) {
	hooks := map[string]any{
		"PostToolUse": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{"type": "command", "command": "/old/bin/yesmem hook-resolve"},
				},
			},
		},
	}
	addPostToolUseResolveHook(hooks, "/new/bin/yesmem")

	cmds := hookCommands(hooks, "PostToolUse")
	if countContaining(cmds, "/new/bin/yesmem hook-resolve") != 1 {
		t.Fatalf("hook-resolve command not updated after the binary moved: %v", cmds)
	}
}

func TestAddSessionEndHook_KeepsForeignMatcherInSharedGroup(t *testing.T) {
	foreign := "/home/user/hooks/session-end-notify.sh"
	hooks := map[string]any{
		"SessionEnd": []any{
			map[string]any{
				"matcher": "clear",
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
					map[string]any{"type": "command", "command": "/usr/local/bin/yesmem session-end"},
				},
			},
		},
	}
	addSessionEndHook(hooks, "/usr/local/bin/yesmem")

	entry := hooks["SessionEnd"].([]any)[0].(map[string]any)
	if entry["matcher"] != "clear" {
		t.Fatalf("matcher = %v, want 'clear' — dropping it widens the foreign handler sharing this group",
			entry["matcher"])
	}
}

func TestAddSessionEndHook_IdempotentWithUnnamedBinary(t *testing.T) {
	hooks := map[string]any{}
	addSessionEndHook(hooks, "/opt/bin/ym")
	addSessionEndHook(hooks, "/opt/bin/ym")

	if n := countContaining(hookCommands(hooks, "SessionEnd"), "session-end"); n != 1 {
		t.Fatalf("SessionEnd registrations = %d, want 1 (commands: %v)", n, hookCommands(hooks, "SessionEnd"))
	}
}

func TestAddIdleTickHook_KeepsForeignHandlerNamedLikeLegacy(t *testing.T) {
	foreign := "/opt/tools/micro-reminder-notify.sh"
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
					map[string]any{"type": "command", "command": "/usr/local/bin/yesmem micro-reminder"},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign handler named like the legacy command was dropped: %v", cmds)
	}
	if countContaining(cmds, "micro-reminder") != 1 {
		t.Fatalf("expected only the foreign micro-reminder handler to remain, got %v", cmds)
	}
	if n := countContaining(cmds, "idle-tick"); n != 1 {
		t.Fatalf("idle-tick registrations = %d, want 1 (commands: %v)", n, cmds)
	}
}

func TestAddIdleTickHook_LeavesForeignCommandByteIdentical(t *testing.T) {
	foreign := `/home/user/bin/hook.sh --msg "a;b"`
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/usr/local/bin/yesmem")

	if countContaining(hookCommands(hooks, "UserPromptSubmit"), foreign) != 1 {
		t.Fatalf("foreign command was reformatted: %v", hookCommands(hooks, "UserPromptSubmit"))
	}
}

func TestAddIdleTickHook_DropsStaleLegacyWhenAlreadyRegistered(t *testing.T) {
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": "/usr/local/bin/yesmem idle-tick"},
				},
			},
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": "/old/place/yesmem micro-reminder"},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if countContaining(cmds, "micro-reminder") != 0 {
		t.Fatalf("stale legacy entry survived because idle-tick was already registered: %v", cmds)
	}
	if n := countContaining(cmds, "idle-tick"); n != 1 {
		t.Fatalf("idle-tick registrations = %d, want 1 (commands: %v)", n, cmds)
	}
}

func TestWriteAuthJSON_FileModeIs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	home := t.TempDir()
	if err := writeAuthJSON(home, map[string]string{"deepseek": "sk-test-key"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(home, ".local", "share", "opencode", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("auth.json mode = %04o, want 0600 — the file carries provider keys in cleartext", got)
	}
}

func TestWriteAuthJSON_TightensExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(`{"openai":{"key":"sk-old"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := writeAuthJSON(home, map[string]string{"deepseek": "sk-test-key"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("auth.json mode = %04o, want 0600 — os.WriteFile keeps the mode an existing file already had", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries map[string]map[string]string
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("auth.json is not valid JSON after the merge: %v", err)
	}
	if entries["openai"]["key"] != "sk-old" || entries["deepseek"]["key"] != "sk-test-key" {
		t.Fatalf("provider entries were not merged: %v", entries)
	}
}

func TestWriteSettingsJSON_TightensExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"ANTHROPIC_API_KEY":"sk-ant-old"}}`), 0664); err != nil {
		t.Fatal(err)
	}

	settings := map[string]any{"env": map[string]any{"ANTHROPIC_API_KEY": "sk-ant-old"}}
	if err := writeSettingsJSON(home, settings); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("settings.json mode = %04o, want 0600 — the env block carries ANTHROPIC_API_KEY", got)
	}
}

func TestAddSessionStartHook_KeepsForeignHandlerNamedLikeYesmem(t *testing.T) {
	foreign := "/home/user/bin/my-yesmem-notes.sh"
	hooks := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "startup",
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addSessionStartHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "SessionStart")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign SessionStart handler was overwritten: %v", cmds)
	}
	if n := countContaining(cmds, "briefing-hook"); n != 1 {
		t.Fatalf("briefing-hook registrations = %d, want 1 (commands: %v)", n, cmds)
	}
}

func TestAddIdleTickHook_RegistersDespiteForeignIdleTickName(t *testing.T) {
	foreign := "/opt/tools/idle-tick-notify.sh"
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign handler was rewritten: %v", cmds)
	}
	if countContaining(cmds, "/usr/local/bin/yesmem idle-tick") != 1 {
		t.Fatalf("idle-tick counted as registered because a foreign command mentions idle-tick: %v", cmds)
	}
}

func TestAddIdleTickHook_KeepsForeignSegmentMentioningYesmem(t *testing.T) {
	foreign := "/home/user/bin/my-yesmem-notes.sh micro-reminder"
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign segment was dropped by the legacy migration: %v", cmds)
	}
}

func TestAddPreToolUseHook_RegistersDespiteForeignHookCheckName(t *testing.T) {
	foreign := "/home/user/bin/hook-check-notify.sh"
	hooks := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addPreToolUseHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "PreToolUse")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign PreToolUse handler was adopted: %v", cmds)
	}
	if countContaining(cmds, "/usr/local/bin/yesmem hook-check") != 1 {
		t.Fatalf("hook-check counted as registered because a foreign command mentions it: %v", cmds)
	}
	if entry := hooks["PreToolUse"].([]any)[0].(map[string]any); entry["matcher"] != "Bash" {
		t.Fatalf("foreign matcher was rewritten to %v", entry["matcher"])
	}
}

func TestAddPreToolUseGuardHook_RegistersDespiteForeignHookGuardName(t *testing.T) {
	foreign := "/home/user/bin/hook-guard-notify.sh"
	hooks := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addPreToolUseGuardHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "PreToolUse")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign PreToolUse handler was adopted: %v", cmds)
	}
	if countContaining(cmds, "/usr/local/bin/yesmem hook-guard") != 1 {
		t.Fatalf("hook-guard counted as registered because a foreign command mentions it: %v", cmds)
	}
	if entry := hooks["PreToolUse"].([]any)[0].(map[string]any); entry["matcher"] != "Bash" {
		t.Fatalf("foreign matcher was rewritten to %v", entry["matcher"])
	}
}

func TestAddPostToolUseFailureCombinedHook_RegistersDespiteForeignHookFailureName(t *testing.T) {
	foreign := "/home/user/bin/hook-failure-notify.sh"
	hooks := map[string]any{
		"PostToolUseFailure": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addPostToolUseFailureCombinedHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "PostToolUseFailure")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign PostToolUseFailure handler was adopted: %v", cmds)
	}
	if countContaining(cmds, "/usr/local/bin/yesmem hook-failure") != 1 {
		t.Fatalf("hook-failure counted as registered because a foreign command mentions it: %v", cmds)
	}
}

func TestAddPostToolUseResolveHook_RegistersDespiteForeignHookResolveName(t *testing.T) {
	foreign := "/home/user/bin/hook-resolve-notify.sh"
	hooks := map[string]any{
		"PostToolUse": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	addPostToolUseResolveHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "PostToolUse")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("foreign PostToolUse handler was adopted: %v", cmds)
	}
	if countContaining(cmds, "/usr/local/bin/yesmem hook-resolve") != 1 {
		t.Fatalf("hook-resolve counted as registered because a foreign command mentions it: %v", cmds)
	}
}

func TestRemoveThinkHook_KeepsForeignHookThinkName(t *testing.T) {
	foreign := "/home/user/bin/hook-think-notify.sh"
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign},
				},
			},
		},
	}
	removeThinkHook(hooks, "")

	if countContaining(hookCommands(hooks, "UserPromptSubmit"), foreign) != 1 {
		t.Fatalf("foreign hook-think handler was removed: %v", hookCommands(hooks, "UserPromptSubmit"))
	}
}

func TestAddSessionStartHook_KeepsCodemapHook(t *testing.T) {
	codemap := "/usr/local/bin/yesmem codemap-hook"
	hooks := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "startup|resume|clear|compact",
				"hooks": []any{
					map[string]any{"type": "command", "command": codemap},
				},
			},
		},
	}
	addSessionStartHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "SessionStart")
	if countContaining(cmds, codemap) != 1 {
		t.Fatalf("the codemap-hook handler was rewritten to a briefing hook: %v", cmds)
	}
	if countContaining(cmds, "briefing-hook") != 1 {
		t.Fatalf("briefing-hook registrations = %d, want 1 (commands: %v)", countContaining(cmds, "briefing-hook"), cmds)
	}
}

func TestAddIdleTickHook_KeepsForeignSegmentsWhenRetargeting(t *testing.T) {
	foreign := "/home/user/bin/my-own-hook.sh"
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign + "; /old/place/yesmem idle-tick"},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/new/bin/yesmem")

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if len(cmds) != 1 {
		t.Fatalf("expected a single handler, got %v", cmds)
	}
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("retargeting dropped the foreign segment of the chain: %v", cmds)
	}
	if countContaining(cmds, "/new/bin/yesmem idle-tick") != 1 {
		t.Fatalf("idle-tick was not retargeted to the new binary: %v", cmds)
	}
}

func TestAddPreToolUseHook_KeepsHandlerArgumentsWhenRetargeting(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{"type": "command", "command": "/old/bin/yesmem hook-check --verbose"},
				},
			},
		},
	}
	addPreToolUseHook(hooks, "/new/bin/yesmem")

	cmds := hookCommands(hooks, "PreToolUse")
	if countContaining(cmds, "/new/bin/yesmem hook-check --verbose") != 1 {
		t.Fatalf("retargeting dropped the handler's own arguments: %v", cmds)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected a single handler, got %v", cmds)
	}
}

func TestAddSessionStartHook_KeepsForeignSegmentsWhenRetargeting(t *testing.T) {
	foreign := "/home/user/bin/my-own-hook.sh"
	hooks := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "startup",
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign + "; /old/place/yesmem briefing-hook"},
				},
			},
		},
	}
	addSessionStartHook(hooks, "/new/bin/yesmem")

	cmds := hookCommands(hooks, "SessionStart")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("retargeting dropped the foreign segment of the chain: %v", cmds)
	}
	if countContaining(cmds, "/new/bin/yesmem briefing-hook") != 1 {
		t.Fatalf("briefing-hook was not retargeted to the new binary: %v", cmds)
	}
}

func TestAddIdleTickHook_RepairsMovedBinaryPath(t *testing.T) {
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": "/old/place/yesmem idle-tick"},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/new/bin/yesmem")

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if countContaining(cmds, "/new/bin/yesmem idle-tick") != 1 {
		t.Fatalf("idle-tick kept the dead path of a moved binary: %v", cmds)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected a single handler, got %v", cmds)
	}
}

func TestAddIdleTickHook_IdempotentWithVerbInsideBinaryPath(t *testing.T) {
	binaryPath := "/home/a idle-tick b/yesmem"
	hooks := map[string]any{}
	addIdleTickHook(hooks, binaryPath)
	addIdleTickHook(hooks, binaryPath)

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if len(cmds) != 1 || cmds[0] != binaryPath+" idle-tick" {
		t.Fatalf("retargeting grew the command instead of keeping it: %v", cmds)
	}
}

func TestAddSessionEndHook_RepairsMovedBinaryPath(t *testing.T) {
	hooks := map[string]any{}
	addSessionEndHook(hooks, "/old/bin/yesmem")
	addSessionEndHook(hooks, "/new/bin/yesmem")

	cmds := hookCommands(hooks, "SessionEnd")
	if countContaining(cmds, "/new/bin/yesmem session-end") != 1 {
		t.Fatalf("session-end kept the dead path of a moved binary: %v", cmds)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected a single handler, got %v", cmds)
	}
}

func TestAddPreToolUseHook_RepairsMovedBinaryPath(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{"type": "command", "command": "/old/bin/yesmem hook-check"},
				},
			},
		},
	}
	addPreToolUseHook(hooks, "/new/bin/yesmem")

	cmds := hookCommands(hooks, "PreToolUse")
	if countContaining(cmds, "/new/bin/yesmem hook-check") != 1 {
		t.Fatalf("hook-check kept the dead path of a moved binary: %v", cmds)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected a single handler, got %v", cmds)
	}
}

func TestAddPreToolUseGuardHook_RepairsMovedBinaryPath(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": hookGuardMatcher,
				"hooks": []any{
					map[string]any{"type": "command", "command": "/old/bin/yesmem hook-guard"},
				},
			},
		},
	}
	addPreToolUseGuardHook(hooks, "/new/bin/yesmem")

	cmds := hookCommands(hooks, "PreToolUse")
	if countContaining(cmds, "/new/bin/yesmem hook-guard") != 1 {
		t.Fatalf("hook-guard kept the dead path of a moved binary: %v", cmds)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected a single handler, got %v", cmds)
	}
}

func TestAddPostToolUseFailureCombinedHook_RepairsMovedBinaryPath(t *testing.T) {
	hooks := map[string]any{
		"PostToolUseFailure": []any{
			map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{"type": "command", "command": "/old/bin/yesmem hook-failure"},
				},
			},
		},
	}
	addPostToolUseFailureCombinedHook(hooks, "/new/bin/yesmem")

	cmds := hookCommands(hooks, "PostToolUseFailure")
	if countContaining(cmds, "/new/bin/yesmem hook-failure") != 1 {
		t.Fatalf("hook-failure kept the dead path of a moved binary: %v", cmds)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected a single handler, got %v", cmds)
	}
}

func TestAddIdleTickHook_KeepsQuotedSemikolonInForeignSegment(t *testing.T) {
	foreign := `/home/user/bin/hook.sh --msg "a;b"`
	hooks := map[string]any{
		"UserPromptSubmit": []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": foreign + "; /old/place/yesmem micro-reminder"},
				},
			},
		},
	}
	addIdleTickHook(hooks, "/usr/local/bin/yesmem")

	cmds := hookCommands(hooks, "UserPromptSubmit")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("the legacy migration reformatted a foreign segment: %v", cmds)
	}
}

func TestAddIdleTickHook_IdempotentWithBinaryPathContainingSpaces(t *testing.T) {
	binaryPath := "/home/a b/.local/bin/yesmem"
	hooks := map[string]any{}
	addIdleTickHook(hooks, binaryPath)
	addIdleTickHook(hooks, binaryPath)

	if n := countContaining(hookCommands(hooks, "UserPromptSubmit"), "idle-tick"); n != 1 {
		t.Fatalf("idle-tick registrations = %d, want 1 (binary path with a space, commands: %v)", n, hookCommands(hooks, "UserPromptSubmit"))
	}
}

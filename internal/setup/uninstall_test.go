package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCleanSettingsJSON_RemovesMCPPermissions(t *testing.T) {
	// Simulate settings with yesmem MCP tool permissions
	allow := []any{"Bash(git:*)", "WebSearch"}
	for _, name := range mcpToolNames {
		allow = append(allow, "mcp__yesmem__"+name)
	}
	settings := map[string]any{
		"permissions": map[string]any{
			"allow":       allow,
			"defaultMode": "acceptEdits",
		},
	}

	cleanMCPPermissions(settings)

	perms := settings["permissions"].(map[string]any)
	cleaned := perms["allow"].([]any)

	// Only non-yesmem entries should remain
	for _, v := range cleaned {
		s := v.(string)
		if strings.HasPrefix(s, "mcp__yesmem__") {
			t.Errorf("yesmem permission not removed: %s", s)
		}
	}

	// Existing non-yesmem entries preserved
	if len(cleaned) != 2 {
		t.Fatalf("expected 2 non-yesmem entries, got %d: %v", len(cleaned), cleaned)
	}
	if cleaned[0] != "Bash(git:*)" || cleaned[1] != "WebSearch" {
		t.Fatalf("non-yesmem entries not preserved: %v", cleaned)
	}
}

func TestCleanSettingsJSON_RemovesWildcard(t *testing.T) {
	settings := map[string]any{
		"permissions": map[string]any{
			"allow": []any{"mcp__yesmem__*", "mcp__yesmem__search", "WebSearch"},
		},
	}

	cleanMCPPermissions(settings)

	cleaned := settings["permissions"].(map[string]any)["allow"].([]any)
	for _, v := range cleaned {
		s := v.(string)
		if strings.HasPrefix(s, "mcp__yesmem__") {
			t.Errorf("yesmem entry not removed: %s", s)
		}
	}
	if len(cleaned) != 1 || cleaned[0] != "WebSearch" {
		t.Fatalf("expected [WebSearch], got %v", cleaned)
	}
}

func TestCleanSettingsJSON_NoPermissions(t *testing.T) {
	settings := map[string]any{}

	// Must not panic
	cleanMCPPermissions(settings)
}

func TestCleanSettingsJSON_AllYesmemPermissions_EmptyArray(t *testing.T) {
	settings := map[string]any{
		"permissions": map[string]any{
			"allow": []any{"mcp__yesmem__search", "mcp__yesmem__remember"},
		},
	}

	cleanMCPPermissions(settings)

	perms := settings["permissions"].(map[string]any)
	// Must serialize to [] not null — Claude Code rejects null
	out, _ := json.Marshal(perms["allow"])
	if string(out) != "[]" {
		t.Fatalf("expected empty array [], got %s", string(out))
	}
}

func TestCleanSettingsJSON_RemovesStatusLine(t *testing.T) {
	settings := map[string]any{
		"statusLine": map[string]any{
			"type":    "command",
			"command": "yesmem statusline",
		},
	}

	cleanStatusLine(settings, "")

	if _, ok := settings["statusLine"]; ok {
		t.Fatal("statusLine not removed")
	}
}

func TestCleanSettingsJSON_PreservesNonYesmemStatusLine(t *testing.T) {
	settings := map[string]any{
		"statusLine": map[string]any{
			"type":    "command",
			"command": "some-other-tool statusline",
		},
	}

	cleanStatusLine(settings, "")

	if _, ok := settings["statusLine"]; !ok {
		t.Fatal("non-yesmem statusLine should be preserved")
	}
}

func TestCleanSettingsJSON_RemovesSessionEndHook(t *testing.T) {
	hooks := map[string]any{
		"SessionEnd": []any{
			map[string]any{
				"matcher": "clear|compact",
				"hooks": []any{
					map[string]any{"type": "command", "command": "/usr/local/bin/yesmem session-end"},
				},
			},
		},
		"SessionStart": []any{
			map[string]any{
				"matcher": "startup|resume|clear|compact",
				"hooks": []any{
					map[string]any{"type": "command", "command": "/usr/local/bin/yesmem briefing-hook"},
				},
			},
		},
	}

	removeYesmemHook(hooks, "SessionEnd", "")

	if _, ok := hooks["SessionEnd"]; ok {
		t.Fatal("SessionEnd hook not removed")
	}
	// SessionStart untouched
	if _, ok := hooks["SessionStart"]; !ok {
		t.Fatal("SessionStart should not be touched")
	}
}

func TestCleanSettingsJSON_RemovesSessionEndHookFromFile(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionEnd": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/yesmem session-end"},
					},
				},
			},
			"SessionStart": []any{
				map[string]any{
					"matcher": "startup",
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/yesmem briefing-hook"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	if err := cleanSettingsJSON(home); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	hooks, _ := got["hooks"].(map[string]any)
	if _, ok := hooks["SessionEnd"]; ok {
		t.Fatalf("uninstall left a dangling SessionEnd hook: %v", hooks["SessionEnd"])
	}
	if _, ok := hooks["SessionStart"]; ok {
		t.Fatalf("uninstall left a dangling SessionStart hook: %v", hooks["SessionStart"])
	}
}

func TestCleanSettingsJSON_DropsEmptyUserPromptSubmitHandler(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := "/home/user/bin/my-own-hook.sh"
	settings := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": foreign},
					},
				},
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/yesmem idle-tick"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	if err := cleanSettingsJSON(home); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	hooks, _ := got["hooks"].(map[string]any)

	cmds := hookCommands(hooks, "UserPromptSubmit")
	for _, cmd := range cmds {
		if strings.TrimSpace(cmd) == "" {
			t.Fatalf("uninstall left an empty UserPromptSubmit command: %v", hooks["UserPromptSubmit"])
		}
		if strings.Contains(cmd, "yesmem") {
			t.Fatalf("uninstall left a yesmem command: %q", cmd)
		}
	}
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("uninstall lost the foreign handler: %v", hooks["UserPromptSubmit"])
	}
}

func TestRestoreAPIKeyFromState_RestoresPreviousKey(t *testing.T) {
	dir := t.TempDir()
	state := map[string]any{"envAPIKey": "sk-ant-original"}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, "install-state.json"), data, 0644)

	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY": "sk-ant-installed-by-yesmem",
		},
	}
	restoreAPIKeyFromState(dir, settings)

	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_API_KEY"] != "sk-ant-original" {
		t.Fatalf("expected original key restored, got %v", env["ANTHROPIC_API_KEY"])
	}
}

func TestRestoreAPIKeyFromState_RemovesWhenNoPrior(t *testing.T) {
	dir := t.TempDir()
	state := map[string]any{"envAPIKey": nil}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, "install-state.json"), data, 0644)

	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY":  "sk-ant-installed",
			"ANTHROPIC_BASE_URL": "http://localhost:9099",
		},
	}
	restoreAPIKeyFromState(dir, settings)

	env := settings["env"].(map[string]any)
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("API key should be removed when no prior key existed")
	}
	if env["ANTHROPIC_BASE_URL"] != "http://localhost:9099" {
		t.Fatal("other env vars should be preserved")
	}
}

func TestRestoreAPIKeyFromState_NoStateFile(t *testing.T) {
	dir := t.TempDir()
	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY": "sk-ant-something",
		},
	}
	// Must not panic, removes key as safe default
	restoreAPIKeyFromState(dir, settings)

	env := settings["env"].(map[string]any)
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("API key should be removed when no state file exists")
	}
}

func TestRestoreAPIKeyFromState_CreatesEnvBlockWhenRestoring(t *testing.T) {
	dir := t.TempDir()
	state := map[string]any{"envAPIKey": "sk-ant-original"}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, "install-state.json"), data, 0644)

	// env block was deleted by removeProxyEnvVar
	settings := map[string]any{}
	restoreAPIKeyFromState(dir, settings)

	env, ok := settings["env"].(map[string]any)
	if !ok {
		t.Fatal("env block should be created when restoring a key")
	}
	if env["ANTHROPIC_API_KEY"] != "sk-ant-original" {
		t.Fatalf("expected sk-ant-original, got %v", env["ANTHROPIC_API_KEY"])
	}
}

func TestRestoreAPIKeyFromState_NoEnvNoState(t *testing.T) {
	dir := t.TempDir()
	settings := map[string]any{}
	// Must not panic
	restoreAPIKeyFromState(dir, settings)
	if _, ok := settings["env"]; ok {
		t.Fatal("env block should not be created when no state file exists")
	}
}

func TestRestorePrimaryApiKeyFromState_RestoresOAuthAccount(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()

	// Pre-install state had an OAuth account
	state := map[string]any{
		"primaryApiKey": "sk-original",
		"oauthAccount":  map[string]any{"emailAddress": "test@test.com", "displayName": "Test"},
	}
	stateData, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dataDir, "install-state.json"), stateData, 0600)

	// claude.json currently has our API key, no OAuth
	claudeJSON := `{"primaryApiKey":"sk-yesmem-set","numStartups":5}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeJSON), 0600)

	restorePrimaryApiKeyFromState(dataDir, home)

	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	if cfg["primaryApiKey"] != "sk-original" {
		t.Fatalf("expected sk-original, got %v", cfg["primaryApiKey"])
	}
	oa, ok := cfg["oauthAccount"].(map[string]any)
	if !ok {
		t.Fatal("oauthAccount not restored")
	}
	if oa["emailAddress"] != "test@test.com" {
		t.Fatalf("expected test@test.com, got %v", oa["emailAddress"])
	}
}

func TestRemoveProxyEnvVar_SetsRealAPIForKeyUsers(t *testing.T) {
	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY":  "sk-ant-key",
			"ANTHROPIC_BASE_URL": "http://localhost:9099",
		},
	}
	removeProxyEnvVar(settings)
	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Fatalf("expected https://api.anthropic.com, got %v", env["ANTHROPIC_BASE_URL"])
	}
}

func TestRemoveProxyEnvVar_RemovesForNonKeyUsers(t *testing.T) {
	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_BASE_URL": "http://localhost:9099",
		},
	}
	removeProxyEnvVar(settings)
	if _, ok := settings["env"]; ok {
		t.Fatal("env block should be removed when no API key and no other vars")
	}
}

func TestUninstallFlow_RestoreKeyThenSetBaseURL(t *testing.T) {
	dir := t.TempDir()
	state := map[string]any{"envAPIKey": "sk-ant-original"}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, "install-state.json"), data, 0644)

	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY":  "sk-ant-installed",
			"ANTHROPIC_BASE_URL": "http://localhost:9099",
		},
	}
	restoreAPIKeyFromState(dir, settings)
	removeProxyEnvVar(settings)

	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_API_KEY"] != "sk-ant-original" {
		t.Fatalf("expected sk-ant-original, got %v", env["ANTHROPIC_API_KEY"])
	}
	if env["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Fatalf("expected https://api.anthropic.com, got %v", env["ANTHROPIC_BASE_URL"])
	}
}

func TestUninstallFlow_NoKeyRemovesBaseURL(t *testing.T) {
	dir := t.TempDir()
	state := map[string]any{"envAPIKey": nil}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, "install-state.json"), data, 0644)

	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_API_KEY":  "sk-ant-installed",
			"ANTHROPIC_BASE_URL": "http://localhost:9099",
		},
	}
	restoreAPIKeyFromState(dir, settings)
	removeProxyEnvVar(settings)

	env, ok := settings["env"].(map[string]any)
	if !ok {
		return // env block removed entirely, fine
	}
	if _, hasKey := env["ANTHROPIC_API_KEY"]; hasKey {
		t.Fatal("API key should be removed")
	}
	if _, hasURL := env["ANTHROPIC_BASE_URL"]; hasURL {
		t.Fatal("BASE_URL should be removed when no API key")
	}
}

func TestCleanProjectLocalSettings_RemovesYesmemPermissions(t *testing.T) {
	home := t.TempDir()

	// Create a project directory with settings.local.json containing yesmem permissions
	projDir := filepath.Join(home, "myproject", ".claude")
	os.MkdirAll(projDir, 0755)
	settings := map[string]any{
		"permissions": map[string]any{
			"allow": []any{
				"WebSearch",
				"mcp__yesmem__search",
				"mcp__yesmem__remember",
				"Bash(git *)",
			},
		},
	}
	data, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(projDir, "settings.local.json"), data, 0644)

	// Create .claude.json with project entry
	claudeJSON := map[string]any{
		"projects": map[string]any{
			filepath.Join(home, "myproject"): map[string]any{
				"hasTrustDialogAccepted": true,
			},
		},
	}
	cdata, _ := json.Marshal(claudeJSON)
	os.WriteFile(filepath.Join(home, ".claude.json"), cdata, 0600)

	cleanProjectLocalSettings(home)

	result, _ := os.ReadFile(filepath.Join(projDir, "settings.local.json"))
	var got map[string]any
	json.Unmarshal(result, &got)
	perms := got["permissions"].(map[string]any)["allow"].([]any)
	for _, p := range perms {
		if strings.Contains(p.(string), "yesmem") {
			t.Fatalf("yesmem permission should be removed: %v", p)
		}
	}
	if len(perms) != 2 {
		t.Fatalf("expected 2 remaining permissions, got %d: %v", len(perms), perms)
	}
}

func TestCleanProjectLocalSettings_NoProjectsNoPanic(t *testing.T) {
	home := t.TempDir()
	// No .claude.json — must not panic
	cleanProjectLocalSettings(home)
}

func TestRestorePrimaryApiKeyFromState_TightensExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	home := t.TempDir()
	dataDir := t.TempDir()

	state := map[string]any{"primaryApiKey": "sk-original"}
	stateData, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dataDir, "install-state.json"), stateData, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"primaryApiKey":"sk-yesmem-set"}`), 0644); err != nil {
		t.Fatal(err)
	}

	restorePrimaryApiKeyFromState(dataDir, home)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf(".claude.json mode = %04o, want 0600 — the restore writes a key back into the file", got)
	}
}

func TestCleanClaudeJSON_TightensExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"yesmem":{"command":"yesmem"}}}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := cleanClaudeJSON(home); err != nil {
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

func TestCleanClaudeJSON_RemovesYesmemSkillUsage(t *testing.T) {
	home := t.TempDir()
	claudeJSON := `{"mcpServers":{},"skillUsage":{"yesmem-sessions":{"usageCount":3},"yesmem-search":{"usageCount":1},"commit":{"usageCount":5}},"numStartups":10}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeJSON), 0600)

	cleanClaudeJSON(home)

	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	su, ok := cfg["skillUsage"].(map[string]any)
	if !ok {
		t.Fatal("skillUsage should still exist (has non-yesmem entries)")
	}
	if _, has := su["yesmem-sessions"]; has {
		t.Fatal("yesmem-sessions should be removed")
	}
	if _, has := su["yesmem-search"]; has {
		t.Fatal("yesmem-search should be removed")
	}
	if _, has := su["commit"]; !has {
		t.Fatal("non-yesmem skill should be preserved")
	}
	if cfg["numStartups"] != float64(10) {
		t.Fatal("other fields should be preserved")
	}
}

func TestCleanClaudeJSON_RemovesEmptySkillUsage(t *testing.T) {
	home := t.TempDir()
	claudeJSON := `{"skillUsage":{"yesmem-sessions":{"usageCount":3}},"numStartups":5}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeJSON), 0600)

	cleanClaudeJSON(home)

	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	if _, ok := cfg["skillUsage"]; ok {
		t.Fatal("skillUsage should be removed when empty after cleanup")
	}
}

func TestCleanClaudeJSON_NoSkillUsageNoPanic(t *testing.T) {
	home := t.TempDir()
	claudeJSON := `{"numStartups":5}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeJSON), 0600)

	err := cleanClaudeJSON(home)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestorePrimaryApiKeyFromState_RestoresKey(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()

	// Pre-install state had a key
	state := map[string]any{"primaryApiKey": "sk-original"}
	stateData, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dataDir, "install-state.json"), stateData, 0600)

	// claude.json currently has our key
	claudeJSON := `{"primaryApiKey":"sk-yesmem-set","numStartups":5}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeJSON), 0600)

	restorePrimaryApiKeyFromState(dataDir, home)

	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	if cfg["primaryApiKey"] != "sk-original" {
		t.Fatalf("expected sk-original restored, got %v", cfg["primaryApiKey"])
	}
}

func TestSavePreInstallState_SavesPrimaryApiKey(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	existing := `{"primaryApiKey":"sk-original","numStartups":5}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(existing), 0600)

	settings := map[string]any{}
	savePreInstallState(dataDir, settings, home)

	state := loadPreInstallState(dataDir)
	if state == nil {
		t.Fatal("state not saved")
	}
	if state["primaryApiKey"] != "sk-original" {
		t.Fatalf("expected sk-original, got %v", state["primaryApiKey"])
	}
}

func TestCleanSettingsJSON_KeepsForeignHandlerNamedLikeYesmem(t *testing.T) {
	foreign := "/home/user/bin/my-yesmem-notes.sh"
	home := writeSettingsForClean(t, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "startup",
					"hooks": []any{
						map[string]any{"type": "command", "command": foreign},
					},
				},
			},
		},
	})

	if err := cleanSettingsJSON(home); err != nil {
		t.Fatal(err)
	}

	hooks := readHooksAfterClean(t, home)
	cmds := hookCommands(hooks, "SessionStart")
	if countContaining(cmds, foreign) != 1 {
		t.Fatalf("uninstall deleted a foreign handler that merely mentions yesmem: %v", cmds)
	}
}

func TestCleanSettingsJSON_RemovesHandlerOfBinaryWithoutYesmemInPath(t *testing.T) {
	home := writeSettingsForClean(t, map[string]any{
		"mcpServers": map[string]any{
			"yesmem": map[string]any{"command": "/opt/bin/ym", "args": []string{"mcp"}},
		},
		"hooks": map[string]any{
			"SessionEnd": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/opt/bin/ym session-end"},
					},
				},
			},
		},
	})

	if err := cleanSettingsJSON(home); err != nil {
		t.Fatal(err)
	}

	hooks := readHooksAfterClean(t, home)
	if _, ok := hooks["SessionEnd"]; ok {
		t.Fatalf("uninstall left the hook of a binary whose name lacks 'yesmem': %v", hooks["SessionEnd"])
	}
}

func TestCleanSettingsJSON_KeepsForeignStatusLine(t *testing.T) {
	foreign := "/home/user/bin/my-yesmem-statusline.sh"
	home := writeSettingsForClean(t, map[string]any{
		"statusLine": map[string]any{"type": "command", "command": foreign},
	})

	if err := cleanSettingsJSON(home); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	sl, ok := got["statusLine"].(map[string]any)
	if !ok || sl["command"] != foreign {
		t.Fatalf("uninstall deleted a foreign statusLine that merely mentions yesmem: %v", got["statusLine"])
	}
}

func TestRemoveProxyEnvVar_RemovesClaudeCodeRepl(t *testing.T) {
	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_BASE_URL": claudeProxyBaseURL,
			"CLAUDE_CODE_REPL":   "true",
			"ANTHROPIC_API_KEY":  "sk-ant-x",
		},
	}

	removeProxyEnvVar(settings)

	env, _ := settings["env"].(map[string]any)
	if _, ok := env["CLAUDE_CODE_REPL"]; ok {
		t.Fatalf("uninstall left CLAUDE_CODE_REPL in the env block: %v", env)
	}
	if env["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Fatalf("ANTHROPIC_BASE_URL = %v, want the direct API endpoint", env["ANTHROPIC_BASE_URL"])
	}
}

func TestCleanSettingsJSON_RemovesHandlerOfMovedUnnamedBinary(t *testing.T) {
	home := writeSettingsForClean(t, map[string]any{
		"mcpServers": map[string]any{
			"yesmem": map[string]any{"command": "/old/bin/ym", "args": []string{"mcp"}},
		},
		"hooks": map[string]any{
			"SessionEnd": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/new/bin/ym session-end"},
					},
				},
			},
		},
	})

	if err := cleanSettingsJSON(home); err != nil {
		t.Fatal(err)
	}

	hooks := readHooksAfterClean(t, home)
	if _, ok := hooks["SessionEnd"]; ok {
		t.Fatalf("uninstall left the hook of a moved binary whose name lacks 'yesmem': %v", hooks["SessionEnd"])
	}
}

// writeSettingsForClean writes a settings.json into a fresh home and returns it.
func writeSettingsForClean(t *testing.T, settings map[string]any) string {
	t.Helper()
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

// readHooksAfterClean returns the hooks block of the settings.json in home.
func readHooksAfterClean(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	hooks, _ := got["hooks"].(map[string]any)
	return hooks
}

// A discarded os.UserHomeDir error turns every RemoveAll target into a path
// relative to the working directory, so the guard has to refuse before the
// first deletion.
func TestUninstallHome_RejectsUnusableHome(t *testing.T) {
	t.Run("empty HOME", func(t *testing.T) {
		t.Setenv("HOME", "")
		if got, err := UserHome(); err == nil {
			t.Fatalf("UserHome() = %q, nil; want an error for an empty HOME", got)
		}
	})
	t.Run("root HOME", func(t *testing.T) {
		t.Setenv("HOME", string(filepath.Separator))
		if got, err := UserHome(); err == nil {
			t.Fatalf("UserHome() = %q, nil; want an error for the root directory", got)
		}
	})
	t.Run("relative HOME", func(t *testing.T) {
		t.Setenv("HOME", "home/chief")
		if got, err := UserHome(); err == nil {
			t.Fatalf("UserHome() = %q, nil; want an error for a relative HOME", got)
		}
	})
	t.Run("usable HOME", func(t *testing.T) {
		want := t.TempDir()
		t.Setenv("HOME", want)
		got, err := UserHome()
		if err != nil {
			t.Fatalf("UserHome: %v", err)
		}
		if got != want {
			t.Errorf("UserHome() = %q, want %q", got, want)
		}
	})
}

// skillUsage is keyed by skill name. Matching the substring "yesmem" removes a
// user's own skill and misses yesmem's own unprefixed skills at the same time.
func TestCleanClaudeJSON_RemovesOnlyYesmemSkills(t *testing.T) {
	home := t.TempDir()
	claudeJSON := `{"skillUsage":{` +
		`"yesmem-remember":{"usageCount":3},` +
		`"yesloop":{"usageCount":2},` +
		`"reddit":{"usageCount":1},` +
		`"reddit_fetch":{"usageCount":6},` +
		`"my-yesmem-helper":{"usageCount":4},` +
		`"commit":{"usageCount":5}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeJSON), 0600); err != nil {
		t.Fatal(err)
	}

	if err := cleanClaudeJSON(home); err != nil {
		t.Fatalf("cleanClaudeJSON: %v", err)
	}

	su := readSkillUsage(t, home)
	// reddit_fetch is the shape a cap-backed skill writes: the tool name, not
	// the directory name.
	for _, gone := range []string{"yesmem-remember", "yesloop", "reddit", "reddit_fetch"} {
		if _, has := su[gone]; has {
			t.Errorf("%s is a yesmem skill and should be removed", gone)
		}
	}
	for _, kept := range []string{"my-yesmem-helper", "commit"} {
		if _, has := su[kept]; !has {
			t.Errorf("%s is not a yesmem skill and must be preserved", kept)
		}
	}
}

func readSkillUsage(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read claude.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("claude.json is not JSON: %v", err)
	}
	su, ok := cfg["skillUsage"].(map[string]any)
	if !ok {
		t.Fatal("skillUsage was removed although foreign entries remained")
	}
	return su
}

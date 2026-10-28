package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultOpencodeSettings_HasRequiredKeys(t *testing.T) {
	s := defaultOpencodeSettings()

	if s["provider"] == nil {
		t.Error("missing provider")
	}
	if s["mcp"] == nil {
		t.Error("missing mcp")
	}
	if s["compaction"] == nil {
		t.Error("missing compaction")
	}

	provider := s["provider"].(map[string]any)
	if provider["deepseek"] == nil {
		t.Error("missing provider.deepseek")
	}
	if provider["openai"] == nil {
		t.Error("missing provider.openai")
	}
	anthropic, ok := provider["anthropic"].(map[string]any)
	if !ok {
		t.Fatal("missing provider.anthropic")
	}
	anthropicOpts, ok := anthropic["options"].(map[string]any)
	if !ok || anthropicOpts["baseURL"] != "http://localhost:9099/v1" {
		t.Errorf("provider.anthropic.options.baseURL should be http://localhost:9099/v1, got %v", anthropic["options"])
	}

	mcp := s["mcp"].(map[string]any)
	yesmem, ok := mcp["yesmem"].(map[string]any)
	if !ok {
		t.Fatal("missing mcp.yesmem")
	}
	if yesmem["type"] != "local" {
		t.Errorf("wrong mcp type: %v", yesmem["type"])
	}
	cmd, ok := yesmem["command"].([]any)
	if !ok || len(cmd) != 2 || cmd[0] != "yesmem" || cmd[1] != "mcp" {
		t.Errorf("wrong mcp command: %v", yesmem["command"])
	}
	env, ok := yesmem["environment"].(map[string]any)
	if !ok || env["YESMEM_SOURCE_AGENT"] != "opencode" {
		t.Errorf("mcp.yesmem.environment.YESMEM_SOURCE_AGENT should be opencode, got %v", yesmem["environment"])
	}
	if yesmem["timeout"] != 60000 {
		t.Errorf("wrong mcp timeout: got %v, want 60000", yesmem["timeout"])
	}

	compaction := s["compaction"].(map[string]any)
	if compaction["auto"] != false {
		t.Errorf("compaction.auto should be false: %v", compaction["auto"])
	}
	if compaction["prune"] != false {
		t.Errorf("compaction.prune should be false: %v", compaction["prune"])
	}
}

func TestRemoveOpencodeProviders(t *testing.T) {
	cfg := map[string]any{
		"provider": map[string]any{
			"deepseek":  map[string]any{"options": map[string]any{"baseURL": "x"}},
			"openai":    map[string]any{"options": map[string]any{"baseURL": "x"}},
			"anthropic": map[string]any{"options": map[string]any{"baseURL": "x"}},
			"custom":    map[string]any{"options": map[string]any{"baseURL": "y"}},
		},
	}

	removeOpencodeProviders(cfg)

	provider := cfg["provider"].(map[string]any)
	if provider["deepseek"] != nil {
		t.Error("deepseek provider not removed")
	}
	if provider["openai"] != nil {
		t.Error("openai provider not removed")
	}
	if provider["anthropic"] != nil {
		t.Error("anthropic provider not removed")
	}
	if provider["custom"] == nil {
		t.Error("custom provider was incorrectly removed")
	}
}

func TestRemoveOpencodeProviders_AllYesMem(t *testing.T) {
	cfg := map[string]any{
		"provider": map[string]any{
			"deepseek":  map[string]any{"options": nil},
			"openai":    map[string]any{"options": nil},
			"anthropic": map[string]any{"options": nil},
		},
	}

	removeOpencodeProviders(cfg)

	if cfg["provider"] != nil {
		t.Error("provider key should be removed when empty")
	}
}

func TestDefaultOpencodeSettings_IncludesOpencodeProvider(t *testing.T) {
	s := defaultOpencodeSettings()
	provider := s["provider"].(map[string]any)

	opencode, ok := provider["opencode"].(map[string]any)
	if !ok {
		t.Fatal("missing provider.opencode")
	}

	if opencode["npm"] != "@ai-sdk/openai-compatible" {
		t.Errorf("provider.opencode.npm should be @ai-sdk/openai-compatible, got %v", opencode["npm"])
	}

	opts, ok := opencode["options"].(map[string]any)
	if !ok {
		t.Fatal("missing provider.opencode.options")
	}
	if opts["baseURL"] != "http://localhost:9099/v1" {
		t.Errorf("provider.opencode.options.baseURL should be http://localhost:9099/v1, got %v", opts["baseURL"])
	}

	models, ok := opencode["models"].(map[string]any)
	if !ok {
		t.Fatal("missing provider.opencode.models")
	}
	bigPickle, ok := models["big-pickle"].(map[string]any)
	if !ok {
		t.Fatal("missing provider.opencode.models.big-pickle")
	}
	if bigPickle["name"] != "Big Pickle" {
		t.Errorf("big-pickle name should be 'Big Pickle', got %v", bigPickle["name"])
	}
}

func TestDefaultOpencodeSettings_OpencodeProviderHasMCPAllowHeader(t *testing.T) {
	s := defaultOpencodeSettings()
	provider := s["provider"].(map[string]any)

	opencode, ok := provider["opencode"].(map[string]any)
	if !ok {
		t.Fatal("missing provider.opencode")
	}

	opts, ok := opencode["options"].(map[string]any)
	if !ok {
		t.Fatal("missing provider.opencode.options")
	}

	headers, ok := opts["headers"].(map[string]any)
	if !ok {
		t.Fatal("missing provider.opencode.options.headers")
	}

	if headers["x-yesmem-allow-mcp"] != "1" {
		t.Errorf("x-yesmem-allow-mcp header should be '1', got %v", headers["x-yesmem-allow-mcp"])
	}
}

func TestRemoveOpencodeProviders_DeletesOpencode(t *testing.T) {
	cfg := map[string]any{
		"provider": map[string]any{
			"opencode": map[string]any{"options": map[string]any{"baseURL": "x"}},
			"custom":   map[string]any{"options": map[string]any{"baseURL": "y"}},
		},
	}

	removeOpencodeProviders(cfg)

	provider := cfg["provider"].(map[string]any)
	if provider["opencode"] != nil {
		t.Error("opencode provider not removed")
	}
	if provider["custom"] == nil {
		t.Error("custom provider was incorrectly removed")
	}
}

func TestRemoveOpencodeMCP(t *testing.T) {
	cfg := map[string]any{
		"mcp": map[string]any{
			"yesmem": map[string]any{"type": "local"},
			"custom": map[string]any{"type": "remote"},
		},
	}

	removeOpencodeMCP(cfg)

	mcp := cfg["mcp"].(map[string]any)
	if mcp["yesmem"] != nil {
		t.Error("yesmem MCP not removed")
	}
	if mcp["custom"] == nil {
		t.Error("custom MCP was incorrectly removed")
	}
}

func TestRemoveOpencodeMCP_OnlyYesMem(t *testing.T) {
	cfg := map[string]any{
		"mcp": map[string]any{
			"yesmem": map[string]any{"type": "local"},
		},
	}

	removeOpencodeMCP(cfg)

	if cfg["mcp"] != nil {
		t.Error("mcp key should be removed when empty")
	}
}

func TestRemoveOpencodeCompaction(t *testing.T) {
	cfg := map[string]any{
		"compaction": map[string]any{
			"auto":      false,
			"threshold": 100000,
		},
	}

	removeOpencodeCompaction(cfg)

	compaction := cfg["compaction"].(map[string]any)
	if compaction["auto"] != nil {
		t.Error("compaction.auto not removed")
	}
	if compaction["threshold"] != 100000 {
		t.Error("compaction.threshold was incorrectly removed")
	}
}

// Uninstall must drop exactly the entry a v1 install wrote. Matching any
// "…/index.ts" with "yesmem" somewhere in the path takes a user's own plugin
// down with it (YM-296).
func TestRemoveOpencodeSettings_DropsOnlyOwnPluginEntry(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	own := filepath.Join(v1InstallDir(home), "index.ts")
	foreign := "/home/other/projects/yesmem-tools/plugins/index.ts"
	other := "/home/other/.config/opencode/plugins/other.ts"
	writeOpencodeJSONC(t, home, `{"plugin": ["`+own+`", "`+foreign+`", "`+other+`"]}`)

	if err := removeOpencodeSettings(home); err != nil {
		t.Fatalf("removeOpencodeSettings: %v", err)
	}

	got := readPluginEntries(t, home)
	want := []string{foreign, other}
	if len(got) != len(want) {
		t.Fatalf("plugin entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("plugin[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRemoveOpencodeSettings_DeletesEmptyPluginKey(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	own := filepath.Join(v1InstallDir(home), "index.ts")
	writeOpencodeJSONC(t, home, `{"plugin": ["`+own+`"]}`)

	if err := removeOpencodeSettings(home); err != nil {
		t.Fatalf("removeOpencodeSettings: %v", err)
	}

	if cfg := readOpencodeConfig(t, home); cfg["plugin"] != nil {
		t.Errorf("plugin key should be removed when empty, got %v", cfg["plugin"])
	}
}

// A config entry only proves a yesmem install when it points at the file a v1
// install wrote: a foreign plugin path with "yesmem" in it reported a working
// installation that does not exist (YM-296).
func TestReadOpencodeConfigState_ForeignYesmemPathIsNotConfigured(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeOpencodeJSONC(t, home, `{"plugin": ["/home/other/projects/yesmem-tools/plugins/index.ts"]}`)

	if state := readOpencodeConfigState(home); state.PluginConfigured {
		t.Error("foreign plugin path reported as a yesmem install")
	}
}

// The narrowing must not cost status the detection of a real v1 leftover.
func TestReadOpencodeConfigState_OwnV1EntryCountsAsConfigured(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	own := filepath.Join(v1InstallDir(home), "index.ts")
	if err := os.MkdirAll(filepath.Dir(own), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("// plugin"), 0644); err != nil {
		t.Fatal(err)
	}
	writeOpencodeJSONC(t, home, `{"plugin": ["`+own+`"]}`)

	if state := readOpencodeConfigState(home); !state.PluginConfigured {
		t.Error("own v1 plugin entry not reported as configured")
	}
}

// A v1 dev setup wrote the plugin path as <bin>/../plugins/opencode-yesmem/
// index.ts, and that shape is ours only while it sits next to the running
// binary (isDevTreeEntry). A bare path in the home is not: before YM-371 it was
// swept together with another project's registration in the same home. The
// entry stays and status does not report a yesmem install.
func TestLos18iRemoveOpencodeSettingsKeepsAForeignDevTreeEntry(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	foreignEntry := filepath.Join(home, "projects", "otherapp", "bin", "..", "plugins", "opencode-yesmem", "index.ts")
	if err := os.MkdirAll(filepath.Dir(foreignEntry), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignEntry, []byte("// plugin"), 0644); err != nil {
		t.Fatal(err)
	}
	userPlugin := filepath.Join(home, ".config", "opencode", "plugins", "other.ts")
	writeOpencodeJSONC(t, home, `{"plugin": ["`+foreignEntry+`", "`+userPlugin+`"]}`)

	// The file exists, so a tail-shaped predicate would report the install as
	// configured. Ownership has to decide: status must not claim it either.
	if state := readOpencodeConfigState(home); state.PluginConfigured {
		t.Error("a foreign dev-tree entry was reported as our install")
	}

	if err := removeOpencodeSettings(home); err != nil {
		t.Fatalf("removeOpencodeSettings: %v", err)
	}

	got := readPluginEntries(t, home)
	if len(got) != 2 || got[0] != foreignEntry || got[1] != userPlugin {
		t.Errorf("plugin entries = %v, want the foreign entry kept", got)
	}
}

// The v2 install directory is the shape that uninstall has to remove from a
// home it owns.
func TestRemoveOpencodeSettings_DropsV2InstallDirEntry(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	v2Entry := filepath.Join(opencodePluginV2Dir(home), "index.ts")
	userPlugin := filepath.Join(home, ".config", "opencode", "plugins", "other.ts")
	writeOpencodeJSONC(t, home, `{"plugin": ["`+v2Entry+`", "`+userPlugin+`"]}`)

	if err := removeOpencodeSettings(home); err != nil {
		t.Fatalf("removeOpencodeSettings: %v", err)
	}

	got := readPluginEntries(t, home)
	if len(got) != 1 || got[0] != userPlugin {
		t.Errorf("plugin entries = %v, want only the user's own plugin", got)
	}
}

// Status counts an entry only while it points at a plugin of ours and the file
// it names exists. The dev shape is not claimed without the binary anchor, so
// this covers the persistent install and the v2 directory instead.
func TestReadOpencodeConfigState_CountsOwnPluginEntries(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	v2Entry := filepath.Join(opencodePluginV2Dir(home), "index.ts")
	if err := os.MkdirAll(filepath.Dir(v2Entry), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v2Entry, []byte("// plugin"), 0644); err != nil {
		t.Fatal(err)
	}
	writeOpencodeJSONC(t, home, `{"plugin": ["`+v2Entry+`"]}`)

	if state := readOpencodeConfigState(home); !state.PluginConfigured {
		t.Error("own v2 plugin entry not reported as configured")
	}
}

func readOpencodeConfig(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(opencodeConfigPath(home))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config after removal is not JSON: %v", err)
	}
	return cfg
}

func readPluginEntries(t *testing.T, home string) []string {
	t.Helper()
	raw, ok := readOpencodeConfig(t, home)["plugin"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		s, ok := p.(string)
		if !ok {
			t.Fatalf("plugin entry is not a string: %v", p)
		}
		out = append(out, s)
	}
	return out
}

func TestDefaultOpencodeSettings_MergePreservesExisting(t *testing.T) {
	original := `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "deepseek": {
      "options": {
        "baseURL": "https://custom.example.com/v1",
        "apiKey": "sk-custom-deepseek-key"
      }
    }
  },
  "mcp": {
    "custom_mcp": {
      "command": ["custom", "mcp"],
      "enabled": true
    }
  }
}`

	var cfg map[string]any
	if err := json.Unmarshal([]byte(original), &cfg); err != nil {
		t.Fatal(err)
	}

	defaults := defaultOpencodeSettings()
	deepMergeJSON(cfg, defaults)

	// deepseek baseURL and apiKey must NOT be overwritten
	ds := cfg["provider"].(map[string]any)["deepseek"].(map[string]any)
	dsOpts := ds["options"].(map[string]any)
	if dsOpts["baseURL"] != "https://custom.example.com/v1" {
		t.Errorf("existing baseURL was overwritten: %v", dsOpts["baseURL"])
	}
	if dsOpts["apiKey"] != "sk-custom-deepseek-key" {
		t.Errorf("existing apiKey was overwritten: %v", dsOpts["apiKey"])
	}

	// models must have been added
	if ds["models"] == nil {
		t.Error("deepseek.models not added")
	}

	// openai provider must be added
	prov := cfg["provider"].(map[string]any)
	if prov["openai"] == nil {
		t.Error("openai provider not added")
	}

	// yesmem MCP must be added
	mcp := cfg["mcp"].(map[string]any)
	if mcp["yesmem"] == nil {
		t.Error("yesmem MCP not added")
	}
	// custom MCP must be preserved
	if mcp["custom_mcp"] == nil {
		t.Error("custom_mcp was lost")
	}

	// compaction must be added
	if cfg["compaction"] == nil {
		t.Error("compaction not added")
	}
}

func TestOpencodeSettingsEndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	configDir := filepath.Join(home, ".config", "opencode")
	os.MkdirAll(configDir, 0755)

	cfgPath := filepath.Join(configDir, "opencode.json")

	existingConfig := `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "deepseek": {
      "options": {
        "baseURL": "https://my-proxy.com/v1"
      }
    }
  }
}`
	if err := os.WriteFile(cfgPath, []byte(existingConfig), 0644); err != nil {
		t.Fatal(err)
	}

	if err := mergeOpencodeSettings(home); err != nil {
		t.Fatalf("mergeOpencodeSettings: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	// Custom baseURL preserved
	dsOpts := cfg["provider"].(map[string]any)["deepseek"].(map[string]any)["options"].(map[string]any)
	if dsOpts["baseURL"] != "https://my-proxy.com/v1" {
		t.Errorf("custom baseURL was overwritten: %v", dsOpts["baseURL"])
	}

	// Models added
	ds := cfg["provider"].(map[string]any)["deepseek"].(map[string]any)
	if ds["models"] == nil {
		t.Error("models not added")
	}

	// openai provider added
	if cfg["provider"].(map[string]any)["openai"] == nil {
		t.Error("openai provider not added")
	}

	// yesmem MCP added
	if cfg["mcp"].(map[string]any)["yesmem"] == nil {
		t.Error("yesmem MCP not added")
	}

	// compaction added
	if cfg["compaction"] == nil {
		t.Error("compaction not added")
	}

	// Schema preserved
	if cfg["$schema"] != "https://opencode.ai/config.json" {
		t.Errorf("schema lost: %v", cfg["$schema"])
	}
}

func TestOpencodeSettingsRemoveEndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	configDir := filepath.Join(home, ".config", "opencode")
	os.MkdirAll(configDir, 0755)

	cfgPath := filepath.Join(configDir, "opencode.json")

	config := `{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["/other/plugin.ts"],
  "provider": {
    "deepseek": {
      "options": {
        "baseURL": "http://localhost:9099/v1"
      },
      "models": {
        "deepseek-chat": {"name": "DeepSeek V4 Flash"}
      }
    },
    "openai": {
      "options": {
        "baseURL": "http://localhost:9099/v1"
      }
    },
    "custom_provider": {
      "options": {"baseURL": "https://custom.example.com"}
    }
  },
  "mcp": {
    "yesmem": {
      "type": "local",
      "command": ["yesmem", "mcp"],
      "enabled": true
    },
    "custom_mcp": {
      "command": ["custom"],
      "enabled": true
    }
  },
  "compaction": {
    "auto": false,
    "custom_setting": "keep"
  }
}`
	if err := os.WriteFile(cfgPath, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}

	if err := removeOpencodeSettings(home); err != nil {
		t.Fatalf("removeOpencodeSettings: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), "yesmem") {
		t.Errorf("yesmem entries not fully removed:\n%s", string(data))
	}

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	// provider.deepseek and provider.openai should be removed
	prov := cfg["provider"].(map[string]any)
	if prov["deepseek"] != nil {
		t.Error("deepseek provider not removed")
	}
	if prov["openai"] != nil {
		t.Error("openai provider not removed")
	}
	// custom_provider should remain
	if prov["custom_provider"] == nil {
		t.Error("custom_provider was incorrectly removed")
	}

	// mcp.yesmem should be removed
	mcp := cfg["mcp"].(map[string]any)
	if mcp["yesmem"] != nil {
		t.Error("yesmem MCP not removed")
	}
	if mcp["custom_mcp"] == nil {
		t.Error("custom_mcp was incorrectly removed")
	}

	// compaction.auto and compaction.prune should be removed
	compaction := cfg["compaction"].(map[string]any)
	if compaction["auto"] != nil {
		t.Error("compaction.auto not removed")
	}
	if compaction["prune"] != nil {
		t.Error("compaction.prune not removed")
	}
	if compaction["custom_setting"] != "keep" {
		t.Error("compaction.custom_setting was incorrectly removed")
	}
}

func TestOpencodeSettingsRemove_EmptyConfigPreserved(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	configDir := filepath.Join(home, ".config", "opencode")
	os.MkdirAll(configDir, 0755)

	cfgPath := filepath.Join(configDir, "opencode.json")

	config := `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "deepseek": {"options": {"baseURL": "http://localhost:9099/v1"}},
    "openai": {"options": {"baseURL": "http://localhost:9099/v1"}}
  },
  "mcp": {
    "yesmem": {"type": "local", "command": ["yesmem", "mcp"]}
  },
  "compaction": {
    "auto": false
  }
}`
	if err := os.WriteFile(cfgPath, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}

	if err := removeOpencodeSettings(home); err != nil {
		t.Fatalf("removeOpencodeSettings: %v", err)
	}

	// File MUST remain (opencode needs the file present; deleting it would
	// break opencode for users who set it up manually for non-yesmem reasons).
	// yesmem entries are removed, $schema stays.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("opencode.json must still exist after yesmem uninstall: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "yesmem") {
		t.Errorf("yesmem entries should be removed but found 'yesmem' in: %s", body)
	}
	if !strings.Contains(body, "$schema") {
		t.Errorf("$schema should be preserved, got: %s", body)
	}
}

func TestOpencodeSettingsRemove_NoFile(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")

	if err := removeOpencodeSettings(home); err != nil {
		t.Fatalf("removeOpencodeSettings should not error on missing file: %v", err)
	}
}

func TestUpgradeOpencodeTimeout_OldDefault(t *testing.T) {
	cfg := map[string]any{
		"mcp": map[string]any{
			"yesmem": map[string]any{
				"timeout": float64(10000),
			},
		},
	}

	upgradeOpencodeTimeout(cfg)

	yesmem := cfg["mcp"].(map[string]any)["yesmem"].(map[string]any)
	if yesmem["timeout"] != float64(60000) {
		t.Errorf("old timeout not upgraded: got %v, want 60000", yesmem["timeout"])
	}
}

func TestUpgradeOpencodeTimeout_CustomPreserved(t *testing.T) {
	cfg := map[string]any{
		"mcp": map[string]any{
			"yesmem": map[string]any{
				"timeout": float64(60000),
			},
		},
	}

	upgradeOpencodeTimeout(cfg)

	yesmem := cfg["mcp"].(map[string]any)["yesmem"].(map[string]any)
	if yesmem["timeout"] != float64(60000) {
		t.Errorf("custom timeout was changed: got %v, want 60000", yesmem["timeout"])
	}
}

func TestUpgradeOpencodeTimeout_NoMCP(t *testing.T) {
	cfg := map[string]any{}
	upgradeOpencodeTimeout(cfg) // must not panic
}

func TestMergeOpencodeSettings_UpgradesOldTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	configDir := filepath.Join(home, ".config", "opencode")
	os.MkdirAll(configDir, 0755)

	cfgPath := filepath.Join(configDir, "opencode.json")

	existingConfig := `{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "yesmem": {
      "type": "local",
      "command": ["yesmem", "mcp"],
      "enabled": true,
      "timeout": 10000
    }
  }
}`
	if err := os.WriteFile(cfgPath, []byte(existingConfig), 0644); err != nil {
		t.Fatal(err)
	}

	if err := mergeOpencodeSettings(home); err != nil {
		t.Fatalf("mergeOpencodeSettings: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	yesmem := cfg["mcp"].(map[string]any)["yesmem"].(map[string]any)
	if yesmem["timeout"] != float64(60000) {
		t.Errorf("old timeout not upgraded in merge: got %v, want 60000", yesmem["timeout"])
	}
}

// --- opencode.jsonc priority tests ---

func TestOpencodeConfigPath_PrefersJsonc(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	jsonc := filepath.Join(dir, "opencode.jsonc")
	jsonF := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(jsonc, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonF, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	got := opencodeConfigPath(home)
	if got != jsonc {
		t.Errorf("expected %s, got %s", jsonc, got)
	}
}

func TestOpencodeConfigPath_FallbackToJson(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	jsonF := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(jsonF, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	got := opencodeConfigPath(home)
	if got != jsonF {
		t.Errorf("expected %s, got %s", jsonF, got)
	}
}

func TestOpencodeConfigPath_FallbackToConfigJson(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	got := opencodeConfigPath(home)
	if got != cfg {
		t.Errorf("expected %s, got %s", cfg, got)
	}
}

func TestOpencodeConfigPath_CreatesJsoncIfNoneExist(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(dir, "opencode.jsonc")
	got := opencodeConfigPath(home)
	if got != expected {
		t.Errorf("expected %s, got %s", expected, got)
	}
	// Must NOT create the file.
	if _, err := os.Stat(expected); !os.IsNotExist(err) {
		t.Errorf("opencodeConfigPath created the file (expected to defer creation): %v", err)
	}
}

// --- migrateOpencodeJsonToJsonc tests ---

func writeOpencodeJSON(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeOpencodeJSONC(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateOpencodeJsonToJsonc_MigratesYesmemContent(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	writeOpencodeJSON(t, home, `{
	  "$schema": "https://opencode.ai/config.json",
	  "provider": {
	    "deepseek": {"options": {"baseURL": "http://localhost:9099/v1"}}
	  },
	  "mcp": {"yesmem": {"type": "local"}}
	}`)

	if err := migrateOpencodeJsonToJsonc(home); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	jsonc := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	data, err := os.ReadFile(jsonc)
	if err != nil {
		t.Fatalf("jsonc not written: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	prov, ok := cfg["provider"].(map[string]any)
	if !ok || prov["deepseek"] == nil {
		t.Errorf("deepseek provider not migrated: %v", cfg["provider"])
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok || mcp["yesmem"] == nil {
		t.Errorf("mcp.yesmem not migrated: %v", cfg["mcp"])
	}
	// $schema must be preserved at the top.
	if cfg["$schema"] == nil {
		t.Error("$schema missing from migrated jsonc")
	}
}

func TestMigrateOpencodeJsonToJsonc_PreservesJsoncUserContent(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	writeOpencodeJSON(t, home, `{
	  "provider": {
	    "deepseek": {"options": {"baseURL": "http://localhost:9099/v1"}},
	    "openai": {"options": {"baseURL": "http://localhost:9099/v1"}}
	  },
	  "mcp": {"yesmem": {"type": "local"}}
	}`)
	// User has manually edited jsonc with a non-yesmem provider — must not be clobbered.
	writeOpencodeJSONC(t, home, `{
	  "$schema": "https://opencode.ai/config.json",
	  "provider": {
	    "custom-vendor": {"options": {"baseURL": "https://example.com/v1"}}
	  }
	}`)

	before, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.jsonc"))
	if err != nil {
		t.Fatal(err)
	}

	if err := migrateOpencodeJsonToJsonc(home); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("jsonc was modified despite user content being present.\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestMigrateOpencodeJsonToJsonc_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	writeOpencodeJSON(t, home, `{"provider": {"deepseek": {"options": {"baseURL": "http://localhost:9099/v1"}}}}`)
	writeOpencodeJSONC(t, home, `{
	  "$schema": "https://opencode.ai/config.json",
	  "provider": {
	    "deepseek": {"options": {"baseURL": "http://localhost:9099/v1"}}
	  }
	}`)
	before, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateOpencodeJsonToJsonc(home); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("jsonc modified despite already having yesmem content (not idempotent)")
	}
}

func TestMigrateOpencodeJsonToJsonc_NoJsonPresent(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// Only jsonc exists — migration is a no-op.
	writeOpencodeJSONC(t, home, `{"$schema": "https://opencode.ai/config.json"}`)
	before, err := os.ReadFile(filepath.Join(dir, "opencode.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateOpencodeJsonToJsonc(home); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "opencode.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("jsonc modified despite no opencode.json being present")
	}
	// opencode.json must not have been created.
	if _, err := os.Stat(filepath.Join(dir, "opencode.json")); !os.IsNotExist(err) {
		t.Errorf("opencode.json unexpectedly created by migration")
	}
}

// --- config write safety (YM-87) ---
//
// A config that Go cannot parse must never be replaced by the defaults: the
// user's file survives and the merge reports the problem. Before the fix both
// merges treated a parse failure as "empty config" and wrote back only
// yesmem's keys (plus $schema), which deleted the user's providers and MCP
// entries.

func TestMergeOpencodeSettingsWith_LeavesUnparseableConfigUntouched(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	// JSONC with a comment: valid for opencode, invalid for encoding/json.
	writeOpencodeJSONC(t, home, `{
	  // my providers
	  "$schema": "https://opencode.ai/config.json",
	  "model": "deepseek/deepseek-reasoner"
	}`)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	before := readFileOrFail(t, cfgPath)

	if err := mergeOpencodeSettingsWith(home, "", "", ""); err == nil {
		t.Fatal("expected an error for an unparseable config, got nil")
	}

	after := readFileOrFail(t, cfgPath)
	if string(before) != string(after) {
		t.Errorf("config was rewritten despite the parse failure\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestMergeOpencodeSettingsWith_RejectsNonObjectConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeOpencodeJSONC(t, home, `null`)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	before := readFileOrFail(t, cfgPath)

	if err := mergeOpencodeSettingsWith(home, "", "", ""); err == nil {
		t.Fatal("expected an error for a non-object config, got nil")
	}
	if after := readFileOrFail(t, cfgPath); string(before) != string(after) {
		t.Errorf("config was rewritten: %s", after)
	}
}

// A readable, valid config still merges: the guard must not block the normal path.
func TestMergeOpencodeSettingsWith_StillMergesValidConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeOpencodeJSONC(t, home, `{"$schema": "https://opencode.ai/config.json", "model": "my/model"}`)

	if err := mergeOpencodeSettingsWith(home, "", "", ""); err != nil {
		t.Fatalf("merge: %v", err)
	}
	data := readFileOrFail(t, filepath.Join(home, ".config", "opencode", "opencode.jsonc"))
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("merged config is not valid JSON: %v", err)
	}
	if cfg["model"] != "my/model" {
		t.Errorf("user model was overwritten: %v", cfg["model"])
	}
	if cfg["mcp"] == nil {
		t.Error("mcp block missing after merge")
	}
}

// The uninstall follows the same fail-closed policy as the merge paths: a
// config yesmem cannot parse is reported, not silently treated as "nothing to
// remove". Reporting it as a clean uninstall leaves the user with a config
// nobody touched and no hint why.
func TestRemoveOpencodeSettings_ReportsUnparseableConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	// JSONC with a comment: valid for opencode, invalid for encoding/json.
	writeOpencodeJSONC(t, home, `{
	  // hand-written
	  "$schema": "https://opencode.ai/config.json",
	  "provider": {"deepseek": {"options": {"baseURL": "http://localhost:9099/v1"}}}
	}`)
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	before := readFileOrFail(t, cfgPath)

	err := removeOpencodeSettings(home)
	if err == nil {
		t.Fatal("expected an error naming the unparseable config, got nil")
	}
	if !strings.Contains(err.Error(), "opencode.jsonc") {
		t.Errorf("error does not name the config: %v", err)
	}
	if after := readFileOrFail(t, cfgPath); string(after) != string(before) {
		t.Errorf("config rewritten despite the parse failure: %s", after)
	}
}

// Migration writes the jsonc through the same atomic path as every other
// config write: a truncate-in-place write that is interrupted leaves a torn
// file that the fail-closed readers cannot repair. Observable difference: the
// atomic path replaces the directory entry, so the inode changes.
func TestMigrateOpencodeJsonToJsonc_ReplacesTheFileInsteadOfTruncatingIt(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeOpencodeJSON(t, home, `{
	  "$schema": "https://opencode.ai/config.json",
	  "mcp": {"yesmem": {"type": "local"}}
	}`)
	writeOpencodeJSONC(t, home, `{"$schema": "https://opencode.ai/config.json"}`)
	jsonc := filepath.Join(home, ".config", "opencode", "opencode.jsonc")

	before, err := os.Stat(jsonc)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateOpencodeJsonToJsonc(home); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	after, err := os.Stat(jsonc)
	if err != nil {
		t.Fatalf("jsonc gone after migrate: %v", err)
	}

	if os.SameFile(before, after) {
		t.Error("jsonc was written in place; an interrupted write would leave a torn config")
	}
	var cfg map[string]any
	if err := json.Unmarshal(readFileOrFail(t, jsonc), &cfg); err != nil {
		t.Fatalf("migrated jsonc is not valid JSON: %v", err)
	}
	if mcp, ok := cfg["mcp"].(map[string]any); !ok || mcp["yesmem"] == nil {
		t.Errorf("mcp.yesmem not migrated: %v", cfg["mcp"])
	}
}

// A zero-byte config is empty, not broken: uninstall has nothing to remove and
// must not demand that the user "remove comments" from an empty file.
func TestRemoveOpencodeSettings_EmptyConfigIsNotAParseError(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeOpencodeJSONC(t, home, "")

	if err := removeOpencodeSettings(home); err != nil {
		t.Errorf("removeOpencodeSettings on an empty config: %v", err)
	}
	if data := readFileOrFail(t, filepath.Join(home, ".config", "opencode", "opencode.jsonc")); len(data) != 0 {
		t.Errorf("empty config was rewritten: %q", data)
	}
}

// Whitespace only is as blank as zero bytes: no comments to remove, nothing to
// report.
func TestRemoveOpencodeSettings_WhitespaceOnlyConfigIsNotAParseError(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeOpencodeJSONC(t, home, "\n\n  \n")

	if err := removeOpencodeSettings(home); err != nil {
		t.Errorf("removeOpencodeSettings on a whitespace-only config: %v", err)
	}
	if data := readFileOrFail(t, filepath.Join(home, ".config", "opencode", "opencode.jsonc")); string(data) != "\n\n  \n" {
		t.Errorf("whitespace-only config was rewritten: %q", data)
	}
}

// A failed plugin-dir cleanup must not disappear when the config write fails as
// well: both are uninstall problems and the user needs to see both.
func TestRemoveOpencodePlugin_ReportsBothErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not bind as root")
	}
	home := filepath.Join(t.TempDir(), "home")
	pluginsDir := filepath.Join(home, ".config", "opencode", "plugins")
	if err := os.MkdirAll(filepath.Join(pluginsDir, "opencode-yesmem"), 0755); err != nil {
		t.Fatal(err)
	}
	writeOpencodeJSONC(t, home, "{ // hand-written\n}\n")
	// The plugin entry cannot be removed without write permission on its parent.
	if err := os.Chmod(pluginsDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(pluginsDir, 0755) })

	err := removeOpencodePlugin(home)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "opencode.jsonc") {
		t.Errorf("the config error is missing: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the plugin-dir error is missing: %v", err)
	}
}

// An install can be reached through a symlinked HOME (dotfiles, /home on
// another volume). The entry then names the resolved path while uninstall sees
// the link, so a string comparison leaves the entry pointing at a directory
// the same run deletes.
func TestRemoveOpencodeSettings_DropsEntryWrittenUnderTheResolvedHome(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "real-home")
	if err := os.MkdirAll(realHome, 0755); err != nil {
		t.Fatal(err)
	}
	linkHome := filepath.Join(base, "link-home")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Skipf("symlinks not supported here: %v", err)
	}

	entry := filepath.Join(v1InstallDir(realHome), "index.ts")
	writeOpencodeJSONC(t, realHome, `{"plugin": ["`+entry+`"]}`)

	if err := removeOpencodeSettings(linkHome); err != nil {
		t.Fatalf("removeOpencodeSettings: %v", err)
	}

	if got := readPluginEntries(t, linkHome); len(got) != 0 {
		t.Errorf("plugin entries = %v, want none: the entry points at the source tree this run deletes", got)
	}
}

// A plugin entry proves a working install only while the file it names exists.
// The dev-tree shape is accepted from any home, so a leftover entry there
// reports "configured" for a plugin that cannot load.
func TestReadOpencodeConfigState_MissingPluginFileIsNotConfigured(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	entry := filepath.Join(v1InstallDir(home), "index.ts")
	writeOpencodeJSONC(t, home, `{"plugin": ["`+entry+`"]}`)

	if state := readOpencodeConfigState(home); state.PluginConfigured {
		t.Error("plugin entry names a file that does not exist and was reported as configured")
	}
}

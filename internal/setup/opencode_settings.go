package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	opencodepkg "github.com/carsteneu/yesmem/internal/opencode"
)

func defaultOpencodeSettings() map[string]any {
	return map[string]any{
		"model":       "deepseek/deepseek-reasoner",
		"small_model": "deepseek/deepseek-chat",
		"provider": map[string]any{
			"deepseek": map[string]any{
				"options": map[string]any{
					"baseURL": "http://localhost:9099/v1",
				},
				"models": map[string]any{
					"deepseek-chat": map[string]any{
						"name":  "DeepSeek V4 Flash",
						"limit": map[string]any{"context": 1000000, "output": 8192},
					},
					"deepseek-reasoner": map[string]any{
						"name":  "DeepSeek V4 Pro",
						"limit": map[string]any{"context": 1000000, "output": 65536},
						"interleaved": map[string]any{
							"field": "reasoning_content",
						},
					},
				},
			},
			"openai": map[string]any{
				"options": map[string]any{
					"baseURL": "http://localhost:9099/v1",
				},
				"models": map[string]any{
					"gpt-5.5": map[string]any{
						"name":  "GPT-5.5",
						"limit": map[string]any{"context": 400000, "output": 128000},
					},
				},
			},
			"anthropic": map[string]any{
				"options": map[string]any{
					"baseURL": "http://localhost:9099/v1",
				},
			},
			"opencode": map[string]any{
				"npm": "@ai-sdk/openai-compatible",
				"options": map[string]any{
					"baseURL": "http://localhost:9099/v1",
					"headers": map[string]any{
						"x-yesmem-allow-mcp": "1",
					},
				},
				"models": map[string]any{
					"big-pickle": map[string]any{
						"name":  "Big Pickle",
						"limit": map[string]any{"context": 200000, "output": 8192},
					},
				},
			},
		},
		"mcp": map[string]any{
			"yesmem": map[string]any{
				"type":    "local",
				"command": []any{"yesmem", "mcp"},
				"enabled": true,
				"timeout": 60000,
				"environment": map[string]any{
					"YESMEM_SOURCE_AGENT": "opencode",
				},
			},
		},
		"compaction": map[string]any{
			"auto":  false,
			"prune": false,
		},
	}
}

// opencodeConfigPath returns the path to the opencode config file opencode
// actually reads. Mirrors opencode's own globalConfigFile() priority
// (opencode.jsonc > opencode.json > config.json); falls back to opencode.jsonc
// when none exist.
//
// Delegates to the shared internal/opencode package so internal/proxy can
// use the same resolution without importing internal/setup (which would
// create a cycle via daemon/httpapi).
func opencodeConfigPath(home string) string {
	return opencodepkg.ConfigPath(home)
}

// migrateOpencodeJsonToJsonc copies yesmem-managed content from an existing
// opencode.json to opencode.jsonc when the jsonc file is empty or contains
// only $schema. The user's manual jsonc edits are preserved via deepMergeJSON
// (existing keys in jsonc win; only missing yesmem-managed keys are filled in
// from opencode.json). Idempotent: no-op if jsonc already has yesmem provider
// blocks or MCP config.
//
// Triggered once during yesmem setup when:
//   - opencode.json exists with yesmem-managed providers (deepseek/openai/anthropic/opencode), and
//   - opencode.jsonc is missing OR contains only $schema.
//
// After migration, opencode.json is left in place as a backup (not deleted).
// yesmem writes future config updates to opencode.jsonc via opencodeConfigPath.
func migrateOpencodeJsonToJsonc(home string) error {
	dir := filepath.Join(home, ".config", "opencode")
	jsonPath := filepath.Join(dir, "opencode.json")
	jsoncPath := filepath.Join(dir, "opencode.jsonc")

	jsonData, err := os.ReadFile(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read opencode.json: %w", err)
	}
	var jsonCfg map[string]any
	if err := json.Unmarshal(jsonData, &jsonCfg); err != nil {
		return nil
	}
	if !hasYesmemManagedContent(jsonCfg) {
		return nil
	}

	jsoncData, err := os.ReadFile(jsoncPath)
	if err != nil && !os.IsNotExist(err) {
		return nil
	}
	var jsoncCfg map[string]any
	if len(jsoncData) > 0 {
		if uerr := json.Unmarshal(jsoncData, &jsoncCfg); uerr != nil {
			return nil
		}
	}
	if hasYesmemManagedContent(jsoncCfg) {
		return nil
	}
	if hasUserContentBeyondSchema(jsoncCfg) {
		return nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	target := jsoncCfg
	if target == nil {
		target = map[string]any{}
	}
	if _, ok := target["$schema"]; !ok {
		if schema, ok := jsonCfg["$schema"]; ok {
			target["$schema"] = schema
		} else {
			target["$schema"] = "https://opencode.ai/config.json"
		}
	}

	yesmemContent := extractYesmemManaged(jsonCfg)
	deepMergeJSON(target, yesmemContent)

	// Same writer as every other config path: temp file plus rename. A
	// truncate-in-place write that is interrupted leaves a torn jsonc, and the
	// fail-closed readers cannot repair it.
	return writeOpencodeConfig(jsoncPath, target)
}

// hasYesmemManagedContent returns true if cfg contains any provider, MCP, or
// compaction block managed by yesmem (deepseek/openai/anthropic/opencode
// providers, mcp.yesmem, or compaction.auto/prune).
func hasYesmemManagedContent(cfg map[string]any) bool {
	if cfg == nil {
		return false
	}
	if prov, ok := cfg["provider"].(map[string]any); ok {
		for _, name := range []string{"deepseek", "openai", "anthropic", "opencode"} {
			if _, ok := prov[name]; ok {
				return true
			}
		}
	}
	if mcp, ok := cfg["mcp"].(map[string]any); ok {
		if _, ok := mcp["yesmem"]; ok {
			return true
		}
	}
	if cmp, ok := cfg["compaction"].(map[string]any); ok {
		if _, ok := cmp["auto"]; ok {
			return true
		}
	}
	return false
}

// extractYesmemManaged returns a new map containing only the yesmem-managed
// keys from src (provider subset, mcp.yesmem, compaction.auto/prune, model,
// small_model, plugin). Used by migration to copy yesmem defaults into jsonc
// without dragging along unrelated user content from opencode.json.
func extractYesmemManaged(src map[string]any) map[string]any {
	out := map[string]any{}
	if v, ok := src["model"]; ok {
		out["model"] = v
	}
	if v, ok := src["small_model"]; ok {
		out["small_model"] = v
	}
	if prov, ok := src["provider"].(map[string]any); ok {
		yesmemProv := map[string]any{}
		for _, name := range []string{"deepseek", "openai", "anthropic", "opencode"} {
			if v, ok := prov[name]; ok {
				yesmemProv[name] = v
			}
		}
		if len(yesmemProv) > 0 {
			out["provider"] = yesmemProv
		}
	}
	if mcp, ok := src["mcp"].(map[string]any); ok {
		if ymcp, ok := mcp["yesmem"]; ok {
			out["mcp"] = map[string]any{"yesmem": ymcp}
		}
	}
	if cmp, ok := src["compaction"].(map[string]any); ok {
		yesmemCmp := map[string]any{}
		if v, ok := cmp["auto"]; ok {
			yesmemCmp["auto"] = v
		}
		if v, ok := cmp["prune"]; ok {
			yesmemCmp["prune"] = v
		}
		if len(yesmemCmp) > 0 {
			out["compaction"] = yesmemCmp
		}
	}
	if plugins, ok := src["plugin"].([]any); ok && len(plugins) > 0 {
		out["plugin"] = plugins
	}
	return out
}

// hasUserContentBeyondSchema returns true if cfg has any key other than
// $schema. Used to gate migration: if the user has manually edited jsonc
// beyond the fresh-install $schema stub, we don't risk clobbering.
func hasUserContentBeyondSchema(cfg map[string]any) bool {
	if cfg == nil {
		return false
	}
	for k := range cfg {
		if k != "$schema" {
			return true
		}
	}
	return false
}

func mergeOpencodeSettings(home string) error {
	return mergeOpencodeSettingsWith(home, "", "", "")
}

// readOpencodeConfigObject loads an opencode config as a JSON object. A missing
// file yields an empty map; anything unreadable or unparseable is an error. The
// merge paths write the object back, so a config they could not parse must not
// become an empty map: that would delete every key yesmem does not manage
// (JSONC with comments is the common case).
func readOpencodeConfigObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseOpencodeConfigObject(path, data)
}

// parseOpencodeConfigObject is the parse half, split out for the callers that
// already hold the bytes and need to tell a blank file from a broken one.
func parseOpencodeConfigObject(path string, data []byte) (map[string]any, error) {
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w (yesmem leaves a config it cannot read untouched; remove comments or fix the syntax)", path, err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("%s does not contain a JSON object (yesmem leaves it untouched)", path)
	}
	return cfg, nil
}

func mergeOpencodeSettingsWith(home, model, smallModel, binaryPath string) error {
	cfgPath := opencodeConfigPath(home)

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	cfg, err := readOpencodeConfigObject(cfgPath)
	if err != nil {
		return err
	}

	defaults := defaultOpencodeSettings()
	if model != "" {
		defaults["model"] = model
	}
	if smallModel != "" {
		defaults["small_model"] = smallModel
	}
	// Use full binary path for the MCP command so opencode finds yesmem
	// even when ~/.local/bin is not yet in the PATH.
	if binaryPath != "" {
		if mcp, ok := defaults["mcp"].(map[string]any); ok {
			if yesmem, ok := mcp["yesmem"].(map[string]any); ok {
				yesmem["command"] = []any{binaryPath, "mcp"}
			}
		}
	}
	deepMergeJSON(cfg, defaults)

	upgradeOpencodeTimeout(cfg)

	cfg["$schema"] = "https://opencode.ai/config.json"

	return writeOpencodeConfig(cfgPath, cfg)
}

func upgradeOpencodeTimeout(cfg map[string]any) {
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		return
	}
	yesmem, ok := mcp["yesmem"].(map[string]any)
	if !ok {
		return
	}
	timeout, ok := yesmem["timeout"].(float64)
	if ok && timeout == 10000 {
		yesmem["timeout"] = float64(60000)
	}
}

func removeOpencodeSettings(home string) error {
	cfgPath := opencodeConfigPath(home)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// A blank config holds nothing of ours, and it is not a syntax problem worth
	// reporting: the parse error would tell the user to remove comments from an
	// empty file.
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}

	// Same policy as the merge paths: a config that cannot be parsed is
	// reported, not treated as empty. Writing the empty map back would delete
	// every key yesmem does not manage, and reporting success would leave the
	// user with an untouched config and no hint why.
	cfg, err := parseOpencodeConfigObject(cfgPath, data)
	if err != nil {
		return err
	}

	removeOpencodeProviders(cfg)
	removeOpencodeMCP(cfg)
	removeOpencodeCompaction(cfg)
	dropYesmemPluginEntry(cfg, home)

	// Always write the cleaned file back. Never delete opencode.json/jsonc even
	// if only $schema remains — opencode needs the file present (it falls back
	// to built-in defaults without one, but users who set up the file manually
	// for their own reasons would lose that work). An empty shell with $schema
	// is harmless and matches what opencode itself creates on fresh install.
	return writeOpencodeConfig(cfgPath, cfg)
}

func removeOpencodeProviders(cfg map[string]any) {
	provider, ok := cfg["provider"].(map[string]any)
	if !ok {
		return
	}
	delete(provider, "deepseek")
	delete(provider, "openai")
	delete(provider, "anthropic")
	delete(provider, "opencode")
	if len(provider) == 0 {
		delete(cfg, "provider")
	}
}

func removeOpencodeMCP(cfg map[string]any) {
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		return
	}
	delete(mcp, "yesmem")
	if len(mcp) == 0 {
		delete(cfg, "mcp")
	}
}

func removeOpencodeCompaction(cfg map[string]any) {
	compaction, ok := cfg["compaction"].(map[string]any)
	if !ok {
		return
	}
	delete(compaction, "auto")
	delete(compaction, "prune")
	if len(compaction) == 0 {
		delete(cfg, "compaction")
	}
}

// opencodePluginsRelDir is where install writes the plugin entries and uninstall
// removes them, relative to the home directory. The announcement renders its
// text from this constant, so a moved directory does not leave the text behind.
const opencodePluginsRelDir = ".config/opencode/plugins"

// opencodePluginDirEntries are the entries under opencodePluginsRelDir that
// yesmem installs and uninstall removes. The uninstall announcement is built
// from this list, so it cannot name fewer entries than the deletion removes.
var opencodePluginDirEntries = []string{"yesmem.ts", "opencode-yesmem", "yesmem-window"}

func removeOpencodePlugin(home string) error {
	// Only yesmem's own entries: <config>/opencode/plugins holds plugins the
	// user added, and removing the whole directory would delete those too.
	// A path that cannot be removed must not stop the rest of the uninstall,
	// so the first error is carried and reported at the end.
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	pluginsDir := filepath.Join(home, opencodePluginsRelDir)
	for _, name := range opencodePluginDirEntries {
		// The list is the authority for what RemoveAll receives, so a value that
		// is not a plain entry name must not be followed out of the directory.
		if !isPlainEntryName(name) {
			keep(fmt.Errorf("refusing to remove opencode plugin entry %q: not a plain name", name))
			continue
		}
		keep(os.RemoveAll(filepath.Join(pluginsDir, name)))
	}

	if err := RemoveThemes(home); err != nil {
		// Like the paths above: a leftover theme file must not stop the
		// settings cleanup.
		fmt.Printf("⚠ removing themes: %v\n", err)
	}

	// The v1 source tree is what a plugin entry in the config points at, so it
	// goes only once the config is proven rewritable. Removing it after a failed
	// parse leaves the entry dangling and the install unrecoverable. The v2
	// directory above is discovered by name and goes regardless: a config that
	// cannot be parsed cannot have its stale entry dropped either way.
	v1SourceDir := filepath.Join(home, filepath.Dir(v1PluginPersistentRelPath))
	cfgErr := removeOpencodeSettings(home)
	if cfgErr == nil {
		keep(os.RemoveAll(v1SourceDir))
	} else if _, statErr := os.Stat(v1SourceDir); statErr == nil {
		fmt.Printf("keeping %s: the opencode config could not be rewritten\n", v1SourceDir)
	}
	return errors.Join(firstErr, cfgErr)
}

type opencodeConfigState struct {
	ConfigPresent        bool
	PluginConfigured     bool
	ProviderConfigured   bool
	MCPConfigured        bool
	CompactionConfigured bool
}

func readOpencodeConfigState(home string) opencodeConfigState {
	state := opencodeConfigState{}

	// The v2 plugin is a directory opencode discovers on its own, so it never
	// appears in the config: check it before the config-derived early returns,
	// or a config yesmem cannot parse reports a working install as missing.
	if fi, err := os.Stat(filepath.Join(home, opencodePluginsRelDir, "opencode-yesmem", "index.ts")); err == nil && !fi.IsDir() {
		state.PluginConfigured = true
	}

	data, err := os.ReadFile(opencodeConfigPath(home))
	if err != nil {
		return state
	}

	state.ConfigPresent = true

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return state
	}

	// Only an entry that points at a v1 install proves a yesmem plugin. Matching
	// the substring "yesmem" reported a user's own plugin as our install. The
	// named file has to exist: an entry that outlived its copy reports an
	// install opencode cannot load.
	if plugins, ok := cfg["plugin"].([]any); ok {
		for _, p := range plugins {
			s, ok := p.(string)
			if !ok || !isV1PluginEntry(s, home) {
				continue
			}
			if fi, err := os.Stat(filepath.Clean(s)); err == nil && !fi.IsDir() {
				state.PluginConfigured = true
				break
			}
		}
	}

	if provider, ok := cfg["provider"].(map[string]any); ok {
		if provider["deepseek"] != nil || provider["openai"] != nil {
			state.ProviderConfigured = true
		}
	}

	if mcp, ok := cfg["mcp"].(map[string]any); ok {
		if mcp["yesmem"] != nil {
			state.MCPConfigured = true
		}
	}

	if compaction, ok := cfg["compaction"].(map[string]any); ok {
		if auto, ok := compaction["auto"].(bool); ok && !auto {
			state.CompactionConfigured = true
		}
	}

	return state
}

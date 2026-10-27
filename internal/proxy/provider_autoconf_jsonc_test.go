package proxy

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// los39WriteHome lays out a throwaway HOME with the three files auto-discovery
// reads. The live configuration is never touched.
func los39WriteHome(t *testing.T, models string, configName, config string, auth string) string {
	t.Helper()
	home := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	mustWrite(filepath.Join(".cache", "opencode", "models.json"), models)
	if configName != "" {
		mustWrite(filepath.Join(".config", "opencode", configName), config)
	}
	if auth != "" {
		mustWrite(filepath.Join(".local", "share", "opencode", "auth.json"), auth)
	}
	t.Setenv("HOME", home)
	return home
}

const los39ModelsJSON = `{
  "deepseek": {"id":"deepseek","npm":"@ai-sdk/openai-compatible","api":"https://api.deepseek.com",
    "models":{"deepseek-chat":{"id":"deepseek-chat"}}},
  "anthropic": {"id":"anthropic","npm":"@ai-sdk/anthropic","api":"",
    "models":{"claude-opus-5-5":{"id":"claude-opus-5-5"}}},
  "google": {"id":"google","npm":"@ai-sdk/google","api":"",
    "models":{"gemini-3-pro":{"id":"gemini-3-pro"}}}
}`

const los39AuthJSON = `{"deepseek":{"type":"api","key":"sk-ds"},"anthropic":{"type":"api","key":"sk-an"},"google":{"type":"api","key":"sk-g"}}`

// los39JSONCConfig is valid JSONC: a line comment, a block comment, and keys in
// an order that is deliberately not alphabetical.
func los39JSONCConfig() string {
	return `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    // deepseek must keep its comment
    "deepseek": {
      "options": {
        "timeout": 600000
      }
    },
    /* anthropic stays Anthropic-compatible */
    "anthropic": {
      "apiKey": "sk-an"
    },
    "google": {
      "apiKey": "sk-g"
    }
  },
  "model": "deepseek/deepseek-chat"
}`
}

// YM-145: the auto-configuration rewrote opencode.json through
// map[string]any + json.MarshalIndent. Go sorts map keys, so every run reordered
// the user's config and silently dropped JSONC comments.
func TestMaybePatchOpenCodeBaseURL_PreservesCommentsAndOrder(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", los39JSONCConfig(), los39AuthJSON)
	path := filepath.Join(home, ".config", "opencode", "opencode.jsonc")

	active := []autoDiscoveredProvider{
		{ProviderID: "deepseek", ModelID: "deepseek-chat", UpstreamURL: "https://api.deepseek.com", IsOpenAICompat: true},
	}
	patched, err := maybePatchOpenCodeBaseURL(path, active)
	if err != nil {
		t.Fatalf("maybePatchOpenCodeBaseURL: %v", err)
	}
	if patched != 1 {
		t.Fatalf("patched = %d, want 1", patched)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "// deepseek must keep its comment") {
		t.Errorf("line comment lost:\n%s", got)
	}
	if !strings.Contains(got, "/* anthropic stays Anthropic-compatible */") {
		t.Errorf("block comment lost:\n%s", got)
	}
	// Key order: provider before model, deepseek before anthropic before google.
	provIdx := strings.Index(got, `"provider"`)
	modelIdx := strings.Index(got, `"model"`)
	dsIdx := strings.Index(got, `"deepseek"`)
	anIdx := strings.Index(got, `"anthropic"`)
	ggIdx := strings.Index(got, `"google"`)
	if provIdx < 0 || modelIdx < 0 || dsIdx < 0 || anIdx < 0 || ggIdx < 0 {
		t.Fatalf("config lost keys:\n%s", got)
	}
	if !(provIdx < dsIdx && dsIdx < anIdx && anIdx < ggIdx && ggIdx < modelIdx) {
		t.Errorf("key order changed: provider=%d deepseek=%d anthropic=%d google=%d model=%d\n%s",
			provIdx, dsIdx, anIdx, ggIdx, modelIdx, got)
	}

	// The patch must land in deepseek's options and nowhere else.
	var cfg struct {
		Provider map[string]struct {
			Options map[string]any `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(blankJSONCNoise(data), &cfg); err != nil {
		t.Fatalf("patched config is not valid JSONC: %v\n%s", err, got)
	}
	if cfg.Provider["deepseek"].Options["baseURL"] != "http://localhost:9099/v1" {
		t.Errorf("deepseek baseURL = %v, want the proxy URL\n%s", cfg.Provider["deepseek"].Options["baseURL"], got)
	}
	if _, has := cfg.Provider["anthropic"].Options["baseURL"]; has {
		t.Errorf("anthropic received a baseURL it did not need:\n%s", got)
	}
}

// YM-145: a provider block without an options object must still get one.
func TestMaybePatchOpenCodeBaseURL_CreatesOptionsBlock(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.json", `{
  "provider": {
    "deepseek": {
      "apiKey": "sk-ds"
    }
  }
}`, los39AuthJSON)
	path := filepath.Join(home, ".config", "opencode", "opencode.json")

	active := []autoDiscoveredProvider{
		{ProviderID: "deepseek", ModelID: "deepseek-chat", UpstreamURL: "https://api.deepseek.com", IsOpenAICompat: true},
	}
	patched, err := maybePatchOpenCodeBaseURL(path, active)
	if err != nil || patched != 1 {
		t.Fatalf("patched=%d err=%v, want 1 and no error", patched, err)
	}

	data, _ := os.ReadFile(path)
	var cfg struct {
		Provider map[string]struct {
			Options map[string]any `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("patched config does not parse: %v\n%s", err, data)
	}
	if cfg.Provider["deepseek"].Options["baseURL"] != "http://localhost:9099/v1" {
		t.Errorf("baseURL missing after creating options block:\n%s", data)
	}
}

// YM-145: loadOpenCodeConfig used encoding/json on a .jsonc file. A single
// comment aborted discovery, so auto-configuration silently did nothing.
func TestLoadOpenCodeConfig_ParsesJSONC(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", los39JSONCConfig(), los39AuthJSON)
	_ = home

	cfg, _, err := loadOpenCodeConfig("")
	if err != nil {
		t.Fatalf("loadOpenCodeConfig on JSONC: %v", err)
	}
	for _, pid := range []string{"deepseek", "anthropic", "google"} {
		if _, ok := cfg[pid]; !ok {
			t.Errorf("provider %q missing from parsed JSONC config: %v", pid, cfg)
		}
	}
}

// YM-145: runAutoDiscovery logged len(seen) as the patched count, but `seen` had
// just been rebuilt from the INACTIVE provider list. The log claimed to patch
// providers that were never touched.
func TestRunAutoDiscovery_ReportsActuallyPatchedCount(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", los39JSONCConfig(), los39AuthJSON)
	_ = home

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	targets, err := runAutoDiscovery(logger, nil)
	if err != nil {
		t.Fatalf("patch failed on a writable config: %v", err)
	}
	if targets == nil {
		t.Fatalf("no targets discovered, log:\n%s", buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "patched opencode config: set baseURL for 1 providers") {
		t.Errorf("patch log must report the 1 provider that was actually patched (2 inactive providers must not count):\n%s", out)
	}
}

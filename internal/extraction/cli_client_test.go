package extraction

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

func TestCLIClientImplementsInterface(t *testing.T) {
	var _ LLMClient = (*CLIClient)(nil)
}

func TestAPIClientImplementsInterface(t *testing.T) {
	var _ LLMClient = (*Client)(nil)
}

func TestCLIModelName(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"claude-haiku-4-5-20251001", "haiku"},
		{"claude-sonnet-4-6", "sonnet"},
		{"claude-opus-4-6", "opus"},
		{"some-custom-model", "some-custom-model"},
	}

	for _, tt := range tests {
		c := NewCLIClient("/usr/bin/claude", tt.model, "")
		if got := c.cliModelName(); got != tt.want {
			t.Errorf("cliModelName(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
}

func TestFilterEnv(t *testing.T) {
	env := []string{
		"HOME=/home/user",
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=mcp",
		"PATH=/usr/bin",
	}

	filtered := filterEnv(env, "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT")

	if len(filtered) != 2 {
		t.Fatalf("expected 2 env vars, got %d: %v", len(filtered), filtered)
	}

	for _, e := range filtered {
		if e == "CLAUDECODE=1" || e == "CLAUDE_CODE_ENTRYPOINT=mcp" {
			t.Errorf("should have been filtered: %s", e)
		}
	}
}

func TestNewLLMClientAutoNoKey(t *testing.T) {
	// Auto mode without API key and no claude binary → nil, nil
	client, err := NewLLMClient("auto", "", "model", "/nonexistent/path/to/claude", "")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if client != nil {
		t.Fatalf("expected nil client, got %v", client)
	}
}

func TestNewLLMClientAPI(t *testing.T) {
	client, err := NewLLMClient("api", "sk-test", "claude-haiku-4-5-20251001", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.Name() != "api" {
		t.Errorf("expected 'api', got %q", client.Name())
	}
	if client.Model() != "claude-haiku-4-5-20251001" {
		t.Errorf("expected model ID, got %q", client.Model())
	}
}

func TestNewLLMClientAPINoKey(t *testing.T) {
	_, err := NewLLMClient("api", "", "model", "", "")
	if err == nil {
		t.Fatal("expected error for api without key")
	}
}

func TestNewLLMClientInvalidProvider(t *testing.T) {
	_, err := NewLLMClient("invalid", "", "", "", "")
	if err == nil {
		t.Fatal("expected error for invalid provider")
	}
}

func TestNewLLMClientOpenAI(t *testing.T) {
	client, err := NewLLMClient("openai", "sk-openai", "gpt-5.2", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	oc, ok := client.(*OpenAIClient)
	if !ok {
		t.Fatalf("client type = %T, want *OpenAIClient", client)
	}
	if oc.Name() != "openai" {
		t.Fatalf("Name() = %q, want openai", oc.Name())
	}
	if oc.endpoint != defaultOpenAIResponsesURL {
		t.Fatalf("endpoint = %q, want %q", oc.endpoint, defaultOpenAIResponsesURL)
	}
}

func TestNewLLMClientOpenAICompatible(t *testing.T) {
	client, err := NewLLMClient("openai_compatible", "sk-openai", "gpt-5.2", "", "https://gateway.example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	oc, ok := client.(*OpenAIClient)
	if !ok {
		t.Fatalf("client type = %T, want *OpenAIClient", client)
	}
	if oc.Name() != "openai_compatible" {
		t.Fatalf("Name() = %q, want openai_compatible", oc.Name())
	}
	if oc.endpoint != "https://gateway.example/v1/chat/completions" {
		t.Fatalf("endpoint = %q, want %q", oc.endpoint, "https://gateway.example/v1/chat/completions")
	}
}

func TestCLIClientName(t *testing.T) {
	c := NewCLIClient("claude", "claude-haiku-4-5-20251001", "")
	if c.Name() != "cli" {
		t.Errorf("expected 'cli', got %q", c.Name())
	}
	if c.Model() != "claude-haiku-4-5-20251001" {
		t.Errorf("expected model, got %q", c.Model())
	}
}

func TestAdaptSystemPromptForAgent_ClaudeUnchanged(t *testing.T) {
	prompt := "Distinguish: Did the USER say something, or did CLAUDE suggest it?"
	result := adaptSystemPromptForAgent("claude", prompt)
	if result != prompt {
		t.Errorf("claude agent should not modify prompt: got %q", result)
	}
}

func TestAdaptSystemPromptForAgent_EmptyAgentUnchanged(t *testing.T) {
	prompt := "Distinguish: Did the USER say something, or did CLAUDE suggest it?"
	result := adaptSystemPromptForAgent("", prompt)
	if result != prompt {
		t.Errorf("empty agent should not modify prompt: got %q", result)
	}
}

func TestAdaptSystemPromptForAgent_OpencodeReplacesClaude(t *testing.T) {
	prompt := "Distinguish: Did the USER say something, or did CLAUDE suggest it? Only what the user said, not what Claude suggested."
	result := adaptSystemPromptForAgent("opencode", prompt)
	if contains(result, "CLAUDE") {
		t.Errorf("opencode agent should replace CLAUDE: got %q", result)
	}
	if contains(result, "Claude") {
		t.Errorf("opencode agent should replace Claude: got %q", result)
	}
	if !contains(result, "THE ASSISTANT") {
		t.Errorf("opencode agent should use THE ASSISTANT: got %q", result)
	}
	if !contains(result, "the assistant") {
		t.Errorf("opencode agent should use the assistant: got %q", result)
	}
}

func TestAdaptSystemPromptForAgent_CodexReplacesClaude(t *testing.T) {
	prompt := "not when Claude suggests it"
	result := adaptSystemPromptForAgent("codex", prompt)
	if result == prompt {
		t.Errorf("codex agent should modify prompt: got %q", result)
	}
	if contains(result, "Claude") {
		t.Errorf("codex agent should replace Claude: got %q", result)
	}
}

func TestAdaptSystemPromptForAgent_ClaudeCodeSession(t *testing.T) {
	prompt := "You read an excerpt from a Claude Code session (user + assistant messages)."
	result := adaptSystemPromptForAgent("opencode", prompt)
	if contains(result, "Claude Code session") {
		t.Errorf("opencode agent should replace Claude Code session: got %q", result)
	}
	if !contains(result, "session") {
		t.Errorf("result should still contain 'session': got %q", result)
	}
}

func TestAdaptSystemPromptForAgent_Idempotent(t *testing.T) {
	prompt := "Distinguish: Did the USER say something, or did CLAUDE suggest it?"
	first := adaptSystemPromptForAgent("opencode", prompt)
	second := adaptSystemPromptForAgent("opencode", first)
	if first != second {
		t.Errorf("adapt should be idempotent: first=%q second=%q", first, second)
	}
}

func TestRunDispatch_OpencodeUsesStdin(t *testing.T) {
	c := NewCLIClient("opencode", "deepseek-v4-pro", "opencode")
	if c.sourceAgent != "opencode" {
		t.Fatalf("expected sourceAgent=opencode, got %q", c.sourceAgent)
	}
}

func TestRunDispatch_ClaudeUsesRunClaude(t *testing.T) {
	c := NewCLIClient("claude", "claude-sonnet-4-6", "claude")
	if c.sourceAgent != "claude" {
		t.Fatalf("expected sourceAgent=claude, got %q", c.sourceAgent)
	}
}

func TestStdinArgs_OpencodeIncludesFormatJSON(t *testing.T) {
	c := NewCLIClient("opencode", "deepseek-v4-pro", "opencode")
	args := c.stdinArgs("")
	if len(args) < 3 {
		t.Fatalf("expected at least 3 args, got %d: %v", len(args), args)
	}
	if args[0] != "run" {
		t.Errorf("expected 'run', got %q", args[0])
	}
	if args[1] != "--format" || args[2] != "json" {
		t.Errorf("expected --format json after run, got %v", args[1:3])
	}
}

func TestStdinArgs_OpencodeWithSessionIncludesFormatJSON(t *testing.T) {
	c := NewCLIClient("opencode", "deepseek-v4-pro", "opencode")
	args := c.stdinArgs("ses_test123")
	if len(args) < 5 {
		t.Fatalf("expected at least 5 args, got %d: %v", len(args), args)
	}
	foundSession := false
	foundFormat := false
	for i, a := range args {
		if a == "--session" && i+1 < len(args) && args[i+1] == "ses_test123" {
			foundSession = true
		}
		if a == "--format" && i+1 < len(args) && args[i+1] == "json" {
			foundFormat = true
		}
	}
	if !foundSession {
		t.Errorf("expected --session ses_test123 in args: %v", args)
	}
	if !foundFormat {
		t.Errorf("expected --format json in args: %v", args)
	}
}

func TestParseOpencodeOutput_TextOnly(t *testing.T) {
	input := `{"type":"text","timestamp":123,"sessionID":"ses_x","part":{"text":"Hallo."}}
{"type":"step_finish","timestamp":124,"sessionID":"ses_x","part":{"reason":"stop","tokens":{"total":100,"input":95,"output":5}}}` + "\n"
	got, err := parseOpencodeOutput(input, "test-model", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Hallo." {
		t.Errorf("expected 'Hallo.', got %q", got)
	}
}

func TestParseOpencodeOutput_MultipleTextParts(t *testing.T) {
	input := `{"type":"text","timestamp":123,"sessionID":"ses_x","part":{"text":"Teil 1"}}
{"type":"text","timestamp":124,"sessionID":"ses_x","part":{"text":" Teil 2"}}
{"type":"step_finish","timestamp":125,"sessionID":"ses_x","part":{"reason":"stop","tokens":{"total":200,"input":180,"output":20}}}` + "\n"
	got, err := parseOpencodeOutput(input, "test-model", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Teil 1 Teil 2" {
		t.Errorf("expected 'Teil 1 Teil 2', got %q", got)
	}
}

func TestParseOpencodeOutput_IgnoresNonTextEvents(t *testing.T) {
	input := `{"type":"step_start","timestamp":123,"sessionID":"ses_x","part":{"type":"step-start"}}
{"type":"tool_use","timestamp":124,"sessionID":"ses_x","part":{"tool":"some_tool","callID":"call_00"}}
{"type":"text","timestamp":125,"sessionID":"ses_x","part":{"text":"Antwort"}}
{"type":"step_finish","timestamp":126,"sessionID":"ses_x","part":{"reason":"stop","tokens":{"total":150,"input":140,"output":10}}}` + "\n"
	got, err := parseOpencodeOutput(input, "test-model", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Antwort" {
		t.Errorf("expected 'Antwort', got %q", got)
	}
}

func TestParseOpencodeOutput_EmptyInput(t *testing.T) {
	_, err := parseOpencodeOutput("", "test-model", nil)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParseOpencodeOutput_NoTextParts(t *testing.T) {
	input := `{"type":"step_start","timestamp":123,"sessionID":"ses_x","part":{"type":"step-start"}}
{"type":"step_finish","timestamp":124,"sessionID":"ses_x","part":{"reason":"stop","tokens":{"total":50,"input":50,"output":0}}}` + "\n"
	_, err := parseOpencodeOutput(input, "test-model", nil)
	if err == nil {
		t.Fatal("expected error when no text events present")
	}
}

func TestParseOpencodeOutput_OnUsageCallback(t *testing.T) {
	var capturedModel string
	var capturedIn, capturedOut int
	prevOnUsage := OnUsage
	OnUsage = func(model string, in, out int) {
		capturedModel = model
		capturedIn = in
		capturedOut = out
	}
	defer func() { OnUsage = prevOnUsage }()

	input := `{"type":"text","timestamp":123,"sessionID":"ses_x","part":{"text":"ok"}}
{"type":"step_finish","timestamp":124,"sessionID":"ses_x","part":{"reason":"stop","tokens":{"total":100,"input":42,"output":7,"cache":{"read":10,"write":5}}}}` + "\n"
	got, err := parseOpencodeOutput(input, "t-model", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Errorf("expected 'ok', got %q", got)
	}
	if capturedModel != "t-model" {
		t.Errorf("expected model 't-model', got %q", capturedModel)
	}
	if capturedIn != 48 {
		t.Errorf("expected billable input 48 (42 raw + 5 cache write + 1 for 10 cache reads), got %d", capturedIn)
	}
	if capturedOut != 7 {
		t.Errorf("expected output 7, got %d", capturedOut)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// writeOpencodeJSONForTest writes the given JSON content to
// <home>/.config/opencode/opencode.json so buildOpencodeConfigOverride can
// read it. Returns the home directory.
func writeOpencodeJSONForTest(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", cfgDir, err)
	}
	cfgPath := filepath.Join(cfgDir, "opencode.json")
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", cfgPath, err)
	}
	return home
}

// TestBuildOpencodeConfigOverride_FreeTierBigPickle verifies that a user with
// only the opencode free-tier provider (big-pickle) gets an override whose
// provider block carries the opencode provider with the proxy baseURL and the
// x-yesmem-allow-mcp header. This is the regression case from the scratchpad:
// previously the hardcoded DeepSeek override broke free-tier users.
func TestBuildOpencodeConfigOverride_FreeTierBigPickle(t *testing.T) {
	cfg := `{
  "provider": {
    "opencode": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {"baseURL": "https://opencode.ai/zen/v1"},
      "models": {
        "big-pickle": {"name": "Big Pickle", "limit": {"context": 200000, "output": 8192}}
      }
    }
  }
}`
	home := writeOpencodeJSONForTest(t, cfg)
	got := buildOpencodeConfigOverride(home)

	var parsed struct {
		MCP      map[string]any `json:"mcp"`
		Provider map[string]struct {
			Options struct {
				BaseURL string            `json:"baseURL"`
				Headers map[string]string `json:"headers"`
			} `json:"options"`
			Models map[string]any `json:"models"`
			Npm    string         `json:"npm"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("override is not valid JSON: %v\nraw: %s", err, got)
	}
	if _, ok := parsed.Provider["opencode"]; !ok {
		t.Errorf("override missing 'opencode' provider; got providers=%v\nraw: %s", keys(parsed.Provider), got)
	}
	entry := parsed.Provider["opencode"]
	if entry.Options.BaseURL != yesmemProxyBaseURL {
		t.Errorf("opencode.options.baseURL = %q, want %q", entry.Options.BaseURL, yesmemProxyBaseURL)
	}
	if got := entry.Options.Headers[yesmemAllowMCPHeader]; got != "1" {
		t.Errorf("opencode.options.headers[%q] = %q, want \"1\"", yesmemAllowMCPHeader, got)
	}
	// Models block must be preserved so big-pickle is still callable.
	if _, ok := entry.Models["big-pickle"]; !ok {
		t.Errorf("models.big-pickle missing; got models=%v", keys(entry.Models))
	}
	// npm must be preserved.
	if entry.Npm != "@ai-sdk/openai-compatible" {
		t.Errorf("opencode.npm = %q, want @ai-sdk/openai-compatible (non-options fields must be preserved)", entry.Npm)
	}
	// Original baseURL (opencode.ai/zen) must NOT leak through.
	if entry.Options.BaseURL == "https://opencode.ai/zen/v1" {
		t.Errorf("opencode.options.baseURL still points at opencode.ai — proxy rewrite missing")
	}
}

// TestBuildOpencodeConfigOverride_DeepSeek verifies the DeepSeek-only user
// (the pre-fix primary case) still gets a valid override with the proxy
// baseURL and the x-yesmem-allow-mcp header.
func TestBuildOpencodeConfigOverride_DeepSeek(t *testing.T) {
	cfg := `{
  "provider": {
    "deepseek": {
      "options": {"baseURL": "https://api.deepseek.com/v1"},
      "models": {
        "deepseek-chat": {"name": "DeepSeek V4 Flash"}
      }
    }
  }
}`
	home := writeOpencodeJSONForTest(t, cfg)
	got := buildOpencodeConfigOverride(home)

	var parsed struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string            `json:"baseURL"`
				Headers map[string]string `json:"headers"`
			} `json:"options"`
			Models map[string]any `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("override is not valid JSON: %v\nraw: %s", err, got)
	}
	entry, ok := parsed.Provider["deepseek"]
	if !ok {
		t.Fatalf("override missing 'deepseek' provider; got=%v\nraw: %s", keys(parsed.Provider), got)
	}
	if entry.Options.BaseURL != yesmemProxyBaseURL {
		t.Errorf("deepseek.options.baseURL = %q, want %q", entry.Options.BaseURL, yesmemProxyBaseURL)
	}
	if got := entry.Options.Headers[yesmemAllowMCPHeader]; got != "1" {
		t.Errorf("deepseek.options.headers[%q] = %q, want \"1\"", yesmemAllowMCPHeader, got)
	}
	if _, ok := entry.Models["deepseek-chat"]; !ok {
		t.Errorf("models.deepseek-chat missing; got models=%v", keys(entry.Models))
	}
}

// TestBuildOpencodeConfigOverride_Mixed verifies that a user with multiple
// providers (the realistic case) gets an override where EVERY provider is
// rewritten to the proxy baseURL. This is the robustness guarantee from the
// scratchpad: no per-provider if/else, no provider left unrewritten.
func TestBuildOpencodeConfigOverride_Mixed(t *testing.T) {
	cfg := `{
  "provider": {
    "anthropic": {"options": {"baseURL": "https://api.anthropic.com"}},
    "deepseek": {
      "options": {"baseURL": "https://api.deepseek.com/v1"},
      "models": {"deepseek-reasoner": {"name": "Pro"}}
    },
    "ollama": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {"baseURL": "http://localhost:11434/v1"},
      "models": {"qwen3.6:27b": {"name": "Qwen"}}
    },
    "opencode": {
      "options": {"baseURL": "https://opencode.ai/zen/v1"},
      "models": {"big-pickle": {"name": "Big Pickle"}}
    }
  }
}`
	home := writeOpencodeJSONForTest(t, cfg)
	got := buildOpencodeConfigOverride(home)

	var parsed struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string            `json:"baseURL"`
				Headers map[string]string `json:"headers"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("override is not valid JSON: %v\nraw: %s", err, got)
	}
	wantProviders := []string{"anthropic", "deepseek", "ollama", "opencode"}
	for _, p := range wantProviders {
		entry, ok := parsed.Provider[p]
		if !ok {
			t.Errorf("override missing provider %q; got providers=%v", p, keys(parsed.Provider))
			continue
		}
		if entry.Options.BaseURL != yesmemProxyBaseURL {
			t.Errorf("provider %q: baseURL = %q, want %q", p, entry.Options.BaseURL, yesmemProxyBaseURL)
		}
		if got := entry.Options.Headers[yesmemAllowMCPHeader]; got != "1" {
			t.Errorf("provider %q: headers[%q] = %q, want \"1\"", p, yesmemAllowMCPHeader, got)
		}
	}
	// No extra providers should have been synthesized.
	if got, want := len(parsed.Provider), len(wantProviders); got != want {
		t.Errorf("override has %d providers, want %d (no synthesis); got=%v", got, want, keys(parsed.Provider))
	}
}

// TestBuildOpencodeConfigOverride_Fallback verifies that when opencode.json
// is unreadable (missing or corrupt) the helper returns the legacy DeepSeek-only
// fallback string byte-for-byte. Preserving the exact bytes keeps DeepSeek-only
// users on unchanged behavior when their config is absent.
func TestBuildOpencodeConfigOverride_Fallback(t *testing.T) {
	want := defaultOpencodeFallbackConfig

	// Regression guard: hard-code the original pre-fix string (NOT the const)
	// so accidental edits to defaultOpencodeFallbackConfig are caught. The
	// original bytes were produced by the literal
	//   `{` + mcpCfg + `,` + providerCfg + `}`
	// where mcpCfg and providerCfg were the two hardcoded literals in the
	// pre-dynamic recipe. This test fails if anyone silently changes the
	// fallback's shape.
	t.Run("fallback matches pre-fix literal byte-for-byte", func(t *testing.T) {
		original := `{"mcp":{"yesmem":{"type":"local","command":["yesmem","mcp"],"enabled":true,"timeout":120000,"environment":{"YESMEM_SOURCE_AGENT":"opencode"}}},"provider":{"deepseek":{"options":{"baseURL":"http://localhost:9099/v1","headers":{"x-yesmem-allow-mcp":"1"}}}}}`
		if defaultOpencodeFallbackConfig != original {
			t.Errorf("defaultOpencodeFallbackConfig drifted from pre-fix bytes:\n got: %s\nwant: %s", defaultOpencodeFallbackConfig, original)
		}
		if len(original) != 255 {
			t.Errorf("pre-fix literal length drifted: got %d, want 255 (sanity check)", len(original))
		}
	})

	// Missing opencode.json (home dir contains nothing).
	t.Run("missing file", func(t *testing.T) {
		home := t.TempDir()
		got := buildOpencodeConfigOverride(home)
		if got != want {
			t.Errorf("fallback mismatch:\n got: %s\nwant: %s", got, want)
		}
	})

	// Missing .config/opencode/ directory entirely.
	t.Run("missing dir", func(t *testing.T) {
		home := t.TempDir()
		got := buildOpencodeConfigOverride(filepath.Join(home, "nonexistent"))
		if got != want {
			t.Errorf("fallback mismatch:\n got: %s\nwant: %s", got, want)
		}
	})

	// Corrupt JSON.
	t.Run("corrupt json", func(t *testing.T) {
		home := writeOpencodeJSONForTest(t, "{not valid json")
		got := buildOpencodeConfigOverride(home)
		if got != want {
			t.Errorf("fallback mismatch:\n got: %s\nwant: %s", got, want)
		}
	})

	// Empty provider block (valid JSON, zero providers).
	t.Run("empty providers", func(t *testing.T) {
		home := writeOpencodeJSONForTest(t, `{"provider":{}}`)
		got := buildOpencodeConfigOverride(home)
		if got != want {
			t.Errorf("fallback mismatch:\n got: %s\nwant: %s", got, want)
		}
	})
}

// keys returns the map keys as a slice for test error formatting.
func keys[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBuildOpencodeConfigOverride_NoOptionsKey covers a provider entry that
// has no options key at all — the rewriter must add one rather than crash.
// (Reviewer-suggested edge case: every other test has an options block present.)
func TestBuildOpencodeConfigOverride_NoOptionsKey(t *testing.T) {
	cfg := `{
  "provider": {
    "bare": {
      "models": {"bare-model": {"name": "Bare"}}
    }
  }
}`
	home := writeOpencodeJSONForTest(t, cfg)
	got := buildOpencodeConfigOverride(home)

	var parsed struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string            `json:"baseURL"`
				Headers map[string]string `json:"headers"`
			} `json:"options"`
			Models map[string]any `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("override is not valid JSON: %v\nraw: %s", err, got)
	}
	entry, ok := parsed.Provider["bare"]
	if !ok {
		t.Fatalf("override missing 'bare' provider; got=%v\nraw: %s", keys(parsed.Provider), got)
	}
	if entry.Options.BaseURL != yesmemProxyBaseURL {
		t.Errorf("bare.options.baseURL = %q, want %q (rewriter must add options block)", entry.Options.BaseURL, yesmemProxyBaseURL)
	}
	if got := entry.Options.Headers[yesmemAllowMCPHeader]; got != "1" {
		t.Errorf("bare.options.headers[%q] = %q, want \"1\"", yesmemAllowMCPHeader, got)
	}
	if _, ok := entry.Models["bare-model"]; !ok {
		t.Errorf("models.bare-model missing; the rewriter must preserve non-options fields; got models=%v", keys(entry.Models))
	}
}

// writeFakeCLI writes a shell script that ignores its flags, drains stdin,
// prints out verbatim and exits with code. The CLI transports can then be
// driven without the real claude/opencode binaries.
func writeFakeCLI(t *testing.T, out string, code int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cli")
	script := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nprintf '%%s' '%s'\nexit %d\n", out, code)
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}
	return path
}

// A CLI that exits non-zero has still emitted one step_finish event per step it
// ran, and the provider bills those tokens. The usage must reach the sink before
// the error path returns, otherwise every failed extraction is free.
func TestRunStdin_ReportsUsageOnCLIError(t *testing.T) {
	ndjson := `{"type":"text","part":{"text":"partial answer"}}` + "\n" +
		`{"type":"step_finish","part":{"tokens":{"total":120,"input":100,"output":20}}}` + "\n"
	cli := NewCLIClient(writeFakeCLI(t, ndjson, 1), "test-model", models.SourceAgentOpencode)
	cli.RateLimiter = nil // the shared limiter paces calls and would stall the test

	var calls, gotIn, gotOut int
	_, err := cli.Complete("sys", "msg", withUsageSink(func(in, out int) {
		calls++
		gotIn, gotOut = in, out
	}))
	if err == nil {
		t.Fatal("expected an error from the failing CLI")
	}
	if calls != 1 {
		t.Fatalf("usage reported %d time(s), want 1", calls)
	}
	if gotIn != 100 || gotOut != 20 {
		t.Errorf("usage = (%d, %d), want (100, 20)", gotIn, gotOut)
	}
}

// The claude transport has the same defect class: --output-format json prints an
// envelope with usage even when the run fails, and is_error runs are billed.
func TestRunClaude_ReportsUsageOnCLIError(t *testing.T) {
	payload := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","usage":{"input_tokens":1400,"output_tokens":30}}`
	cli := NewCLIClient(writeFakeCLI(t, payload, 1), "claude-sonnet-4-6", models.SourceAgentClaude)
	cli.RateLimiter = nil

	var calls, gotIn, gotOut int
	_, err := cli.Complete("sys", "msg", withUsageSink(func(in, out int) {
		calls++
		gotIn, gotOut = in, out
	}))
	if err == nil {
		t.Fatal("expected an error from the failing CLI")
	}
	if calls != 1 {
		t.Fatalf("usage reported %d time(s), want 1", calls)
	}
	if gotIn != 1400 || gotOut != 30 {
		t.Errorf("usage = (%d, %d), want (1400, 30)", gotIn, gotOut)
	}
}

// A 0/0 report marks the call as measured in BudgetClient.book, so the char
// estimate that would otherwise book the call is skipped and the call costs
// nothing. Counts that are not real must not reach the sink.
func TestExtractAndReportUsage_SkipsZeroTokens(t *testing.T) {
	payload := `{"type":"result","result":"ok","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}}`
	cli := NewCLIClient("claude", "test-model", models.SourceAgentClaude)

	var calls int
	got := cli.extractAndReportUsage(payload, func(in, out int) { calls++ })
	if got != "ok" {
		t.Errorf("result = %q, want \"ok\"", got)
	}
	if calls != 0 {
		t.Errorf("zero token counts were reported %d time(s); the estimate is suppressed by that", calls)
	}
}

// Same rule for the opencode path: a step that produced no tokens at all must
// not report 0/0, because that drops the whole call from the budget. Cache reads
// are not "nothing": they are priced as input equivalents, so a step whose whole
// prompt was a cache hit does report.
func TestParseOpencodeOutput_NoTokensNotReported(t *testing.T) {
	input := `{"type":"text","part":{"text":"ok"}}` + "\n" +
		`{"type":"step_finish","part":{"tokens":{"total":0,"input":0,"output":0,"cache":{"read":0,"write":0}}}}` + "\n"

	var calls int
	got, err := parseOpencodeOutput(input, "test-model", func(in, out int) { calls++ })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Errorf("text = %q, want \"ok\"", got)
	}
	if calls != 0 {
		t.Errorf("zero token counts were reported %d time(s); the estimate is suppressed by that", calls)
	}
}

// The CLI transports now report usage from envelopes that come from an external
// process. clampTokens is the same guard the HTTP path applies: a negative count
// would credit the budget, a count beyond any context window would block the
// rest of the day.
func TestExtractAndReportUsage_ClampsAbsurdCounts(t *testing.T) {
	payload := `{"type":"result","result":"ok","usage":{"input_tokens":-5000,"output_tokens":999999999}}`
	cli := NewCLIClient("claude", "test-model", models.SourceAgentClaude)

	var calls, gotIn, gotOut int
	cli.extractAndReportUsage(payload, func(in, out int) {
		calls++
		gotIn, gotOut = in, out
	})
	if calls != 1 {
		t.Fatalf("usage reported %d time(s), want 1", calls)
	}
	if gotIn != 0 {
		t.Errorf("negative input tokens reported as %d, want 0", gotIn)
	}
	if gotOut != maxReportedTokens {
		t.Errorf("output tokens reported as %d, want the %d cap", gotOut, maxReportedTokens)
	}
}

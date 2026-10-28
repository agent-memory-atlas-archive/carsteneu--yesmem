package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests here close the gaps the three review subagents found in the first
// version of these fixes (Los 39, Phase 5).

// Cold review finding: replaceJSONStringValue matched the first textual
// occurrence, so a nested reasoning_effort was rewritten while the top-level
// field kept "xhigh" and the provider still answered 400.
func TestReplaceJSONStringValue_TargetsTopLevelField(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-pro","extra_body":{"reasoning_effort":"medium"},"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`)

	out := replaceJSONStringValue(body, "reasoning_effort", "high")

	var got struct {
		ReasoningEffort string `json:"reasoning_effort"`
		ExtraBody       struct {
			ReasoningEffort string `json:"reasoning_effort"`
		} `json:"extra_body"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, out)
	}
	if got.ReasoningEffort != "high" {
		t.Errorf("top-level reasoning_effort = %q, want high\n%s", got.ReasoningEffort, out)
	}
	if got.ExtraBody.ReasoningEffort != "medium" {
		t.Errorf("nested reasoning_effort was rewritten to %q, want medium untouched\n%s", got.ExtraBody.ReasoningEffort, out)
	}
}

// Cold review finding: the value search could hit a string that merely looks
// like the key.
func TestReplaceJSONStringValue_IgnoresKeysInStrings(t *testing.T) {
	body := []byte(`{"system":"note: \"reasoning_effort\":\"xhigh\" is rejected","messages":[],"reasoning_effort":"xhigh"}`)

	out := replaceJSONStringValue(body, "reasoning_effort", "high")

	if !strings.Contains(string(out), `\"reasoning_effort\":\"xhigh\" is rejected`) {
		t.Errorf("string content was modified:\n%s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v, want high", got["reasoning_effort"])
	}
}

// Cold review finding: the config tolerated comments but not the trailing commas
// JSONC also allows, so discovery stayed silently dead for such files.
func TestLoadOpenCodeConfig_ParsesTrailingCommas(t *testing.T) {
	los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", `{
  "provider": {
    "deepseek": {
      "options": {
        "timeout": 600000,
      },
    },
  },
  "model": "deepseek/deepseek-chat",
}`, los39AuthJSON)

	cfg, _, err := loadOpenCodeConfig("")
	if err != nil {
		t.Fatalf("loadOpenCodeConfig with trailing commas: %v", err)
	}
	if _, ok := cfg["deepseek"]; !ok {
		t.Fatalf("deepseek missing: %v", cfg)
	}
}

// Cold review finding: one malformed provider block cancelled the patch for all
// providers, although the other blocks are perfectly patchable.
func TestPatchOpenCodeBaseURLs_SkipsUnusableBlock(t *testing.T) {
	doc := []byte(`{
  "provider": {
    "broken": "not an object",
    "deepseek": {
      "options": {
        "timeout": 1
      }
    }
  }
}`)

	out, patched, err := patchOpenCodeBaseURLs(doc, []string{"broken", "deepseek"})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched != 1 {
		t.Fatalf("patched = %d, want 1 (the usable provider only)", patched)
	}
	if !strings.Contains(string(out), `"baseURL": "http://localhost:9099/v1"`) {
		t.Errorf("deepseek did not receive a baseURL:\n%s", out)
	}
	if !strings.Contains(string(out), `"broken": "not an object"`) {
		t.Errorf("unusable block was modified:\n%s", out)
	}
}

// Security review finding: a duplicated top-level "provider" key made the patch
// land in the dead block while the effective (last-wins) config stayed
// unpatched, and the log still claimed a successful patch.
func TestPatchOpenCodeBaseURLs_RejectsDuplicateProviderKey(t *testing.T) {
	doc := []byte(`{"provider":{"deepseek":{}},"provider":{"other":{"apiKey":"k"}}}`)

	out, patched, err := patchOpenCodeBaseURLs(doc, []string{"deepseek"})
	if err == nil {
		t.Fatalf("expected an error for a duplicated provider key, got patched=%d out=%s", patched, out)
	}
	if out != nil {
		t.Fatalf("a refused patch must not return a document, got %s", out)
	}
}

// Security review finding: duplicated keys one level down (two "options"
// objects) merged during decoding, so the verification saw the patched dead
// block while a last-wins parser would not. The patch must refuse instead.
func TestPatchOpenCodeBaseURLs_RejectsDuplicateOptions(t *testing.T) {
	doc := []byte(`{"provider":{"deepseek":{"options":{"timeout":1},"options":{"timeout":2}}}}`)

	out, patched, err := patchOpenCodeBaseURLs(doc, []string{"deepseek"})
	if err == nil {
		t.Fatalf("expected an error for duplicated options, got patched=%d out=%s", patched, out)
	}
	if out != nil {
		t.Fatalf("a refused patch must not return a document, got %s", out)
	}
}

// Cold review finding: "options": null or an array made the patch create a
// second options key, which then failed verification and aborted EVERY provider.
// A non-object options value must skip that provider only.
func TestPatchOpenCodeBaseURLs_SkipsNonObjectOptions(t *testing.T) {
	doc := []byte(`{
  "provider": {
    "broken": {"options": null},
    "deepseek": {"options": {"timeout": 1}}
  }
}`)

	out, patched, err := patchOpenCodeBaseURLs(doc, []string{"broken", "deepseek"})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched != 1 {
		t.Fatalf("patched = %d, want 1 (only deepseek is patchable)", patched)
	}
	if !strings.Contains(string(out), `"options": {"timeout": 1, "baseURL"`) &&
		!strings.Contains(string(out), `"baseURL": "http://localhost:9099/v1"`) {
		t.Errorf("deepseek did not receive a baseURL:\n%s", out)
	}
	if strings.Count(string(out), `"options"`) != 2 {
		t.Errorf("a second options key was inserted for the null block:\n%s", out)
	}
}

// Consequence check finding: a byte order mark made the whole config
// unparseable, which killed discovery without a trace.
func TestLoadOpenCodeConfig_ToleratesBOM(t *testing.T) {
	los39WriteHome(t, los39ModelsJSON, "opencode.jsonc", "\ufeff"+`{
  "provider": {
    "deepseek": {"options": {"timeout": 1}}
  }
}`, los39AuthJSON)

	cfg, _, err := loadOpenCodeConfig("")
	if err != nil {
		t.Fatalf("loadOpenCodeConfig with BOM: %v", err)
	}
	if _, ok := cfg["deepseek"]; !ok {
		t.Fatalf("deepseek missing: %v", cfg)
	}
}

// Cold review finding: the end-of-line choice was document-wide, so one stray
// CRLF turned an LF file into a mixed one. It now follows the nearest line.
func TestPatchOpenCodeBaseURLs_UsesNearestLineEnding(t *testing.T) {
	doc := []byte("{\n  \"provider\": {\n    \"deepseek\": {\r\n      \"options\": {\r\n        \"timeout\": 1\r\n      }\r\n    }\r\n  }\n}\n")

	out, patched, err := patchOpenCodeBaseURLs(doc, []string{"deepseek"})
	if err != nil || patched != 1 {
		t.Fatalf("patched=%d err=%v", patched, err)
	}
	// The options block is CRLF formatted, so the insertion must be CRLF too.
	if !strings.Contains(string(out), "\"options\": {\r\n        \"baseURL\"") {
		t.Errorf("insertion ignored the local line ending:\n%q", out)
	}
}

// Consequence check finding: CRLF files got LF insertions, so the whole file
// looked rewritten to the next diff.
func TestPatchOpenCodeBaseURLs_KeepsCRLF(t *testing.T) {
	doc := []byte("{\r\n  \"provider\": {\r\n    \"deepseek\": {\r\n      \"options\": {\r\n        \"timeout\": 1\r\n      }\r\n    }\r\n  }\r\n}\r\n")

	out, patched, err := patchOpenCodeBaseURLs(doc, []string{"deepseek"})
	if err != nil || patched != 1 {
		t.Fatalf("patched=%d err=%v, want 1 and no error", patched, err)
	}
	if strings.Count(string(out), "\r\n") != strings.Count(string(doc), "\r\n")+1 {
		t.Errorf("expected exactly one new CRLF line, got:\n%q", out)
	}
	if strings.Count(string(out), "\n") != strings.Count(string(out), "\r\n") {
		t.Errorf("mixed line endings after patch:\n%q", out)
	}
}

// Security review finding: os.WriteFile truncates in place, so a crash could
// leave a half-written user config. The write now goes through a temp file and a
// rename, and the first patch leaves a one-time backup with restrictive mode.
func TestMaybePatchOpenCodeBaseURL_TempWriteAndOneTimeBackup(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.json", `{
  "provider": {
    "deepseek": {
      "options": {}
    }
  }
}`, los39AuthJSON)
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	active := []autoDiscoveredProvider{
		{ProviderID: "deepseek", ModelID: "deepseek-chat", UpstreamURL: "https://api.deepseek.com", IsOpenAICompat: true},
	}
	if patched, err := maybePatchOpenCodeBaseURL(path, active); err != nil || patched != 1 {
		t.Fatalf("patched=%d err=%v", patched, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600 preserved", info.Mode().Perm())
	}
	backup, err := os.ReadFile(path + autoconfBackupSuffix)
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if string(backup) != string(before) {
		t.Errorf("backup is not the pre-patch content:\n%s", backup)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".opencode-patch-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	// Second run: nothing to patch, and the backup must not be overwritten.
	if patched, err := maybePatchOpenCodeBaseURL(path, active); err != nil || patched != 0 {
		t.Fatalf("second run patched=%d err=%v, want 0 and no error", patched, err)
	}
	again, err := os.ReadFile(path + autoconfBackupSuffix)
	if err != nil {
		t.Fatalf("backup disappeared: %v", err)
	}
	if string(again) != string(backup) {
		t.Errorf("backup was rewritten on the second run")
	}

	backupInfo, err := os.Stat(path + autoconfBackupSuffix)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if backupInfo.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v, want 0600 (it carries the provider keys)", backupInfo.Mode().Perm())
	}
}

// Security review finding: the atomic rename replaced a symlinked config with a
// regular file, silently detaching it from dotfile management.
func TestMaybePatchOpenCodeBaseURL_KeepsSymlink(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.json", `{
  "provider": {
    "deepseek": {
      "options": {}
    }
  }
}`, los39AuthJSON)
	link := filepath.Join(home, ".config", "opencode", "opencode.json")
	target := filepath.Join(home, "dotfiles-opencode.json")
	if err := os.Rename(link, target); err != nil {
		t.Fatalf("move config: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	active := []autoDiscoveredProvider{
		{ProviderID: "deepseek", ModelID: "deepseek-chat", UpstreamURL: "https://api.deepseek.com", IsOpenAICompat: true},
	}
	if patched, err := maybePatchOpenCodeBaseURL(link, active); err != nil || patched != 1 {
		t.Fatalf("patched=%d err=%v", patched, err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced by a regular file")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.Contains(string(data), `"baseURL": "http://localhost:9099/v1"`) {
		t.Errorf("the symlink target was not patched:\n%s", data)
	}
	// The backup belongs next to the path the user configured, not next to the
	// target: dotfile repos would otherwise pick up an untracked copy of the keys.
	if _, statErr := os.Stat(link + autoconfBackupSuffix); statErr != nil {
		t.Errorf("backup missing next to the configured path: %v", statErr)
	}
	if _, statErr := os.Stat(target + autoconfBackupSuffix); statErr == nil {
		t.Errorf("backup was written into the symlink target's directory (dotfiles repo)")
	}
}

// Cold review finding: a config the user made read-only was still rewritten,
// because a rename only needs the directory permission.
func TestMaybePatchOpenCodeBaseURL_RespectsReadOnlyConfig(t *testing.T) {
	home := los39WriteHome(t, los39ModelsJSON, "opencode.json", `{
  "provider": {
    "deepseek": {
      "options": {}
    }
  }
}`, los39AuthJSON)
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	active := []autoDiscoveredProvider{
		{ProviderID: "deepseek", ModelID: "deepseek-chat", UpstreamURL: "https://api.deepseek.com", IsOpenAICompat: true},
	}
	patched, err := maybePatchOpenCodeBaseURL(path, active)
	if patched != 0 {
		t.Errorf("patched = %d, want 0 for a read-only config", patched)
	}
	if err == nil {
		t.Error("expected an error explaining why the read-only config was left alone")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read after: %v", readErr)
	}
	if string(after) != string(before) {
		t.Errorf("read-only config was modified:\n%s", after)
	}
	if _, statErr := os.Stat(path + autoconfBackupSuffix); statErr == nil {
		t.Error("a backup was written although nothing was patched")
	}
}

// Cold review finding: the builder and the endpoint used different inputs, so a
// configured fork model reached the endpoint but never the body. The test now
// builds the request instead of only comparing the two helper functions.
func TestForkModel_ReachesBodyAndEndpoint(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":8192,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`)
	ctx := ForkContext{OriginalBody: body, SourceFormat: "openai"}

	if got := ctx.forkModel(ForkConfig{}); got != "claude-opus-5-5" {
		t.Errorf("inherited model = %q, want claude-opus-5-5", got)
	}

	cfg := ForkConfig{Model: "deepseek-chat", MaxTokens: 1024, Prompt: func(ForkContext) string { return "x" }}
	if got := ctx.forkModel(cfg); got != "deepseek-chat" {
		t.Fatalf("explicit model = %q, want deepseek-chat", got)
	}
	if !ctx.forkIsOpenAI(ctx.forkModel(cfg)) {
		t.Error("explicit deepseek model must select the OpenAI format")
	}

	built, err := buildForkRequest(ctx, cfg)
	if err != nil {
		t.Fatalf("buildForkRequest: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(built, &got); err != nil {
		t.Fatalf("unmarshal built body: %v", err)
	}
	if got["model"] != "deepseek-chat" {
		t.Errorf("body model = %v, want deepseek-chat (the model the endpoint was resolved for)", got["model"])
	}
	if model := extractModelFromBody(built); model != ctx.forkModel(cfg) {
		t.Errorf("body model %q and endpoint model %q diverge", model, ctx.forkModel(cfg))
	}
}

// Cold review finding: a configured fork model of another family (a Claude model
// with an OpenAI-format source) must not be spliced into the OpenAI body; the
// clone keeps its own model so body and endpoint still agree. The reverse case
// matters just as much: gpt-5.2 belongs in an OpenAI body.
func TestForkModel_FamilyCheck(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-pro","max_tokens":8192,"messages":[{"role":"user","content":"Hello"}]}`)
	ctx := ForkContext{OriginalBody: body, SourceFormat: "openai"}

	build := func(model string) map[string]any {
		t.Helper()
		cfg := ForkConfig{Model: model, MaxTokens: 1024, Prompt: func(ForkContext) string { return "x" }}
		built, err := buildForkRequest(ctx, cfg)
		if err != nil {
			t.Fatalf("buildForkRequest(%q): %v", model, err)
		}
		var got map[string]any
		if err := json.Unmarshal(built, &got); err != nil {
			t.Fatalf("unmarshal built body: %v", err)
		}
		return got
	}

	if got := build("claude-sonnet-4-6"); got["model"] != "deepseek-v4-pro" {
		t.Errorf("body model = %v, want the cloned model (an Anthropic name does not belong in an OpenAI body)", got["model"])
	}
	for _, model := range []string{"gpt-5.2", "glm-4.6", "kimi-k2"} {
		if got := build(model); got["model"] != model {
			t.Errorf("body model = %v for %q, want the configured model", got["model"], model)
		}
	}
}

package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// YM-97: the schema check reports unknown keys and keys sitting at the wrong
// level. A misplaced proxy key is the dangerous one: it is inert.
func TestCheckKeysReportsUnknownAndMisplaced(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "proxy:\n  token_threshold: 600000\n  frobnicate: 1\ntoken_threshold: 800000\nprovider_targets:\n  deepseek: https://api.deepseek.com\nfrobnicate: 1\nmystery_section:\n  nested: 2\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	issues, err := CheckKeys(cfgPath)
	if err != nil {
		t.Fatalf("CheckKeys: %v", err)
	}

	byKey := map[string]Issue{}
	for _, iss := range issues {
		byKey[iss.Key] = iss
	}
	for _, key := range []string{"token_threshold", "provider_targets", "frobnicate", "proxy.frobnicate", "mystery_section"} {
		if _, ok := byKey[key]; !ok {
			t.Errorf("missing issue for %q, got %+v", key, issues)
		}
	}
	if byKey["token_threshold"].Kind != "misplaced" || byKey["token_threshold"].Suggest != "proxy.token_threshold" {
		t.Errorf("token_threshold issue = %+v, want misplaced -> proxy.token_threshold", byKey["token_threshold"])
	}
	if byKey["provider_targets"].Kind != "misplaced" || byKey["provider_targets"].Suggest != "proxy.provider_targets" {
		t.Errorf("provider_targets issue = %+v, want misplaced -> proxy.provider_targets", byKey["provider_targets"])
	}
	if byKey["proxy.frobnicate"].Kind != "unknown" {
		t.Errorf("proxy.frobnicate issue = %+v, want unknown", byKey["proxy.frobnicate"])
	}
	if _, ok := byKey["mystery_section.nested"]; ok {
		t.Error("children of an unknown section must not be reported one by one")
	}
}

// YM-97: a config that matches the schema produces no issues.
func TestCheckKeysAcceptsValidConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "extraction:\n  model: sonnet\nproxy:\n  enabled: true\n  token_threshold: 600000\n  provider_targets:\n    deepseek: https://api.deepseek.com\n  model_features:\n    deepseek:\n      skill_eval: true\nbriefing:\n  languages:\n    - de\n    - en\nembedding:\n  provider: sse\n  search:\n    ivf_threshold: 50000\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	issues, err := CheckKeys(cfgPath)
	if err != nil {
		t.Fatalf("CheckKeys: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("valid config reported %d issue(s): %+v", len(issues), issues)
	}
}

// YM-97: a missing config file is not an issue.
func TestCheckKeysMissingFile(t *testing.T) {
	issues, err := CheckKeys(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("CheckKeys: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("missing file reported %d issue(s)", len(issues))
	}
}

// dropping it silently, which is how a misplaced token_threshold survived.
func TestLoadWarnsAboutMisplacedTopLevelProxyKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "proxy:\n  token_threshold: 600000\ntoken_threshold: 800000\nprovider_targets:\n  deepseek: https://api.deepseek.com\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureLog(t, func() {
		if _, err := Load(cfgPath); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
	for _, want := range []string{"token_threshold", "provider_targets", "misplaced"} {
		if !strings.Contains(out, want) {
			t.Errorf("load warning missing %q, log was: %s", want, out)
		}
	}
}

// YM-97: a typo is invisible today. Load reports it and keeps running.
func TestLoadWarnsAboutUnknownKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  token_threshold: 600000\nfrobnicate: 1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureLog(t, func() {
		cfg, err := Load(cfgPath)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// A warning must not abort loading.
		if cfg.Proxy.TokenThreshold != 600000 {
			t.Errorf("token_threshold = %d, want 600000", cfg.Proxy.TokenThreshold)
		}
	})
	if !strings.Contains(out, "frobnicate") {
		t.Errorf("load warning missing the unknown key, log was: %s", out)
	}
}

// YM-97: a config that matches the schema stays quiet.
func TestLoadDoesNotWarnAboutValidConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "extraction:\n  model: sonnet\nproxy:\n  token_threshold: 600000\n  provider_targets:\n    deepseek: https://api.deepseek.com\n  token_thresholds:\n    glm-5.2: 500000\nagents:\n  token_budget: 0\npricing:\n  sonnet:\n    input: 3\n    output: 15\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureLog(t, func() {
		if _, err := Load(cfgPath); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
	if strings.Contains(out, "schema") {
		t.Errorf("valid config produced a schema warning: %s", out)
	}
}

// captureLog redirects the standard logger for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	fn()
	return buf.String()
}

func TestNormalizeSetPath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		// Already in schema position: unchanged.
		{in: "proxy.token_threshold", want: "proxy.token_threshold"},
		{in: "extraction.model", want: "extraction.model"},
		{in: "caps_dir", want: "caps_dir"},
		{in: "agents.token_budget", want: "agents.token_budget"},

		// Proxy keys without the prefix move under proxy:.
		{in: "token_threshold", want: "proxy.token_threshold"},
		{in: "cache_keepalive_enabled", want: "proxy.cache_keepalive_enabled"},
		{in: "provider_targets.privateTomMax", want: "proxy.provider_targets.privateTomMax"},
		{in: "model_features.deepseek.assoc_context", want: "proxy.model_features.deepseek.assoc_context"},

		// A map key may contain a dot.
		{in: "token_thresholds.glm-5.2", want: "proxy.token_thresholds.glm-5.2"},
		{in: "pricing.deepseek-v4-flash.input", want: "pricing.deepseek-v4-flash.input"},

		// Unknown keys are rejected instead of written where nothing reads them.
		{in: "frobnicate", wantErr: true},
		{in: "proxy.frobnicate", wantErr: true},
		{in: "proxy.token_threshold.nested", wantErr: true},
		{in: "extraction.frobnicate", wantErr: true},
		{in: "", wantErr: true},
	}

	for _, tt := range tests {
		got, err := NormalizeSetPath(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("NormalizeSetPath(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeSetPath(%q) error = %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("NormalizeSetPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// YM-96: the write boundary itself maps a bare proxy key onto proxy:, so every
// caller lands in the same place, not just the MCP handler.
func TestSetValueWritesBareProxyKeyUnderProxy(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetValue(cfgPath, "token_threshold", 300000); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	got, err := GetValue(cfgPath, "proxy.token_threshold")
	if err != nil {
		t.Fatalf("GetValue: %v", err)
	}
	if got != 300000 {
		t.Errorf("proxy.token_threshold = %v (%T), want 300000", got, got)
	}

	// Assert on the document itself: reading "token_threshold" now resolves to
	// the proxy key by design, so only the raw file can show where it landed.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := raw["token_threshold"]; ok {
		t.Errorf("stray top-level token_threshold in:\n%s", data)
	}
}

// YM-96: an unknown key must not touch the file at all.
func TestSetValueRejectsUnknownKeyWithoutWriting(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	const before = "proxy:\n  enabled: true\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetValue(cfgPath, "frobnicate", 1); err == nil {
		t.Fatal("SetValue accepted an unknown key")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("file changed by a rejected write:\n%s", after)
	}
}

// Cold review F1: a value that does not fit the field type makes Load reject the
// whole file and fall back to defaults, so it must never be written.
func TestSetValueRejectsMistypedValue(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	const before = "proxy:\n  token_threshold: 600000\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetValue(cfgPath, "token_threshold", "opus=500000"); err == nil {
		t.Fatal("SetValue accepted a string for an int key")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("file changed by a rejected write:\n%s", after)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after rejected write: %v", err)
	}
	if cfg.Proxy.TokenThreshold != 600000 {
		t.Errorf("token_threshold = %d, want the untouched 600000", cfg.Proxy.TokenThreshold)
	}
}

// A numeric string still coerces into its field, which is what the MCP handler
// and the CLI hand over.
func TestSetValueCoercesStringIntoFieldType(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetValue(cfgPath, "token_threshold", "300000"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.TokenThreshold != 300000 {
		t.Errorf("token_threshold = %d, want 300000", cfg.Proxy.TokenThreshold)
	}
}

// Cold review F7: reading the bare key must find what writing it wrote.
func TestGetValueReadsBareProxyKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  token_threshold: 300000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := GetValue(cfgPath, "token_threshold")
	if err != nil {
		t.Fatalf("GetValue: %v", err)
	}
	if got != 300000 {
		t.Errorf("GetValue(token_threshold) = %v (%T), want 300000", got, got)
	}

	// An unknown path stays a miss, not an error.
	if got, err = GetValue(cfgPath, "frobnicate"); err != nil {
		t.Errorf("GetValue(unknown) error = %v, want nil", err)
	} else if got != nil {
		t.Errorf("GetValue(unknown) = %v, want nil", got)
	}
}

// Cold review F3: the migration writes twice in one second. The first backup
// must survive, otherwise there is no rollback to the pre-migration file.
func TestBackupFileKeepsBothWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(path, []byte("one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := backupFile(path); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if err := os.WriteFile(path, []byte("two\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := backupFile(path); err != nil {
		t.Fatalf("second backup: %v", err)
	}

	backups, _ := filepath.Glob(path + ".bak.*")
	if len(backups) != 2 {
		t.Fatalf("backups = %v, want two distinct files", backups)
	}
	got := map[string]bool{}
	for _, b := range backups {
		data, err := os.ReadFile(b)
		if err != nil {
			t.Fatal(err)
		}
		got[string(data)] = true
	}
	if !got["one\n"] || !got["two\n"] {
		t.Errorf("backups %v do not contain both states", backups)
	}
}

// Cold review round 2, finding 1: a section target can never be filled from a
// string, and writing it makes the whole file undecodable.
func TestSetValueRejectsSectionTargets(t *testing.T) {
	for _, key := range []string{"proxy.provider_targets", "proxy.token_thresholds", "proxy.claude_prompt", "exclude_projects"} {
		cfgPath := filepath.Join(t.TempDir(), "config.yaml")
		const before = "proxy:\n  token_threshold: 600000\n"
		if err := os.WriteFile(cfgPath, []byte(before), 0644); err != nil {
			t.Fatal(err)
		}
		if err := SetValue(cfgPath, key, "x"); err == nil {
			t.Errorf("SetValue(%q) accepted a scalar for a section", key)
		}
		after, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != before {
			t.Errorf("%q: file changed by a rejected write:\n%s", key, after)
		}
	}
}

// Cold review round 2, finding 2: a model key with a dot must stay one key
// instead of becoming nested maps, otherwise Load cannot decode the file.
func TestSetValueKeepsDottedMapKeyInOneSegment(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetValue(cfgPath, "proxy.token_thresholds.glm-5.2", 500000); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.TokenThresholds["glm-5.2"] != 500000 {
		t.Errorf("token_thresholds = %v, want glm-5.2: 500000", cfg.Proxy.TokenThresholds)
	}

	got, err := GetValue(cfgPath, "proxy.token_thresholds.glm-5.2")
	if err != nil {
		t.Fatalf("GetValue: %v", err)
	}
	if got != 500000 {
		t.Errorf("GetValue = %v (%T), want 500000", got, got)
	}
}

// The same for a map whose values are structs: the dotted key stays whole.
func TestSetValueKeepsDottedKeyUnderStructMap(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetValue(cfgPath, "pricing.deepseek-v4-flash.input", 0.14); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pricing["deepseek-v4-flash"].Input != 0.14 {
		t.Errorf("pricing = %v, want deepseek-v4-flash input 0.14", cfg.Pricing)
	}
}

// Cold review round 2, finding 6: "0"/"1" are common switch values.
func TestSetValueAcceptsZeroOneForBool(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetValue(cfgPath, "proxy.enabled", "0"); err != nil {
		t.Fatalf("SetValue(0): %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.Enabled {
		t.Error("proxy.enabled = true, want false after setting 0")
	}
}

// Cold review round 3, NEW-2: a path that ends on a map entry names a whole
// section. Reading it must work (it did before), writing it must not.
func TestGetValueReadsMapSectionEntry(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "proxy:\n  model_features:\n    deepseek:\n      timestamps: true\npricing:\n  glm-5.2:\n    input: 0.5\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := GetValue(cfgPath, "proxy.model_features.deepseek")
	if err != nil {
		t.Fatalf("GetValue: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok || m["timestamps"] != true {
		t.Errorf("model_features.deepseek = %v (%T), want map[timestamps:true]", got, got)
	}

	// A dotted model name resolves as well.
	if got, err = GetValue(cfgPath, "pricing.glm-5.2"); err != nil {
		t.Fatalf("GetValue(dotted): %v", err)
	}
	if m, ok = got.(map[string]any); !ok || m["input"] != 0.5 {
		t.Errorf("pricing.glm-5.2 = %v (%T), want map[input:0.5]", got, got)
	}

	// Writing such a section is still refused.
	if err := SetValue(cfgPath, "proxy.model_features.deepseek", "x"); err == nil {
		t.Error("SetValue accepted a scalar for a map entry holding a struct")
	}
}

// Cold review round 3, VORBESTEHEND-2: an empty segment would create a key that
// matches every model in the proxy's substring lookup.
func TestSetValueRejectsEmptySegment(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	const before = "proxy:\n  enabled: true\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0644); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"proxy.token_thresholds.", "proxy..token_threshold", ""} {
		if err := SetValue(cfgPath, key, 123456); err == nil {
			t.Errorf("SetValue(%q) accepted an empty segment", key)
		}
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != before {
		t.Errorf("file changed:\n%s", data)
	}
}

// Cold review round 4: the documented model-suffix form must work at the file
// level too, so the CLI and every reader see the same value.
func TestModelThresholdAliasAtFileLevel(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetValue(cfgPath, "token_threshold:deepseek", 500000); err != nil {
		t.Fatalf("SetValue(alias): %v", err)
	}
	for _, path := range []string{"token_threshold:deepseek", "proxy.token_threshold:deepseek", "proxy.token_thresholds.deepseek", "token_thresholds.deepseek"} {
		got, err := GetValue(cfgPath, path)
		if err != nil {
			t.Fatalf("GetValue(%q): %v", path, err)
		}
		if got != 500000 {
			t.Errorf("GetValue(%q) = %v (%T), want 500000", path, got, got)
		}
	}
}

// Security review: a caller-supplied key is foreign input, and the map
// backtracking is quadratic, so the segment count is bounded.
func TestSetValueRejectsLongKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	long := "proxy." + strings.Repeat("a.", 40) + "b"
	if err := SetValue(cfgPath, long, 1); err == nil {
		t.Fatal("SetValue accepted a key with 42 segments")
	}
	if _, err := GetValue(cfgPath, long); err != nil {
		t.Fatalf("GetValue must not error, just miss: %v", err)
	}
}

// Security review: a NaN price makes Check() pass and HasBudget() fail at the
// same time, so the numeric domain is part of the type check.
func TestSetValueRejectsInvalidNumbers(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	const before = "pricing:\n  opus:\n    input: 5\n    output: 25\nproxy:\n  token_threshold: 600000\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ key, value string }{
		{"pricing.opus.input", "NaN"},
		{"pricing.opus.input", "Inf"},
		{"pricing.opus.input", "-5"},
		{"token_threshold", "-1"},
		{"proxy.usage_deflation_factor", "-0.3"},
	} {
		if err := SetValue(cfgPath, tc.key, CoerceValue(tc.value)); err == nil {
			t.Errorf("SetValue(%s=%s) accepted an invalid number", tc.key, tc.value)
		}
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != before {
		t.Errorf("file changed:\n%s", data)
	}
	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// Security review: rule_guard is read by the opencode plugin, not by Load, so it
// must not be reported as an unknown key.
func TestCheckKeysAcceptsPluginSections(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "rule_guard:\n  model: deepseek-v4-flash\nproxy:\n  enabled: true\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	issues, err := CheckKeys(cfgPath)
	if err != nil {
		t.Fatalf("CheckKeys: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("plugin section reported as unknown: %+v", issues)
	}
}

// Security review round 5: config.yaml carries the API keys, so the writer
// tightens the mode of a file that was created world readable.
func TestSetValueWritesConfigFileOwnerOnly(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("proxy:\n  enabled: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetValue(cfgPath, "token_threshold", 300000); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("config.yaml mode = %o, want 600", perm)
	}
}

// Security review round 5: an oversized key is refused before it is split or
// echoed back.
func TestSetValueRejectsOversizedKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	long := "proxy." + strings.Repeat("a", 600)
	err := SetValue(cfgPath, long, 1)
	if err == nil {
		t.Fatal("SetValue accepted a 606 byte key")
	}
	if strings.Contains(err.Error(), strings.Repeat("a", 100)) {
		t.Error("the error message echoes the oversized key")
	}
}

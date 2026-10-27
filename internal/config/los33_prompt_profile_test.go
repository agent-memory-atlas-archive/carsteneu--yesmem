package config

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
	"gopkg.in/yaml.v3"
)

// los33PromptLayer decodes a prompt layer the way config.yaml delivers it:
// only the keys that are actually written exist, so an explicit "false" can
// mean "switch this off" while a missing key means "inherit".
func los33PromptLayer(t *testing.T, doc string) *PromptFlags {
	t.Helper()
	var f PromptFlags
	if err := yaml.Unmarshal([]byte(doc), &f); err != nil {
		t.Fatalf("decode prompt layer: %v", err)
	}
	return &f
}

// YM-136: mergeFlags copied only true values, so a profile layer could add a
// flag but never switch one off. A profile that names a flag as false must
// disable what the shared layer enabled for every profile.
func TestLos33ProfileLayerCanDisableSharedFlag(t *testing.T) {
	cfg := Default()
	cfg.Proxy.SharedPrompt = &PromptFlags{OutputDiscipline: true, Beweislast: true}
	cfg.Proxy.OpencodePrompt = los33PromptLayer(t, "prompt_output_discipline: false\n")

	flags := cfg.Proxy.EffectivePromptFlags(models.ProfileOpencode)
	if flags.OutputDiscipline {
		t.Error("opencode_prompt with prompt_output_discipline: false must disable the shared flag")
	}
	if !flags.Beweislast {
		t.Error("a key the profile does not name must keep inheriting from shared_prompt")
	}
}

// The tri-state must not change the meaning of a layer built in code (no YAML
// decode): there, false still means "no opinion", which is what the flat legacy
// fields rely on.
func TestLos33ProgrammaticLayerKeepsTrueOnlySemantics(t *testing.T) {
	cfg := Default()
	cfg.Proxy.PromptOutputDiscipline = false
	cfg.Proxy.SharedPrompt = &PromptFlags{OutputDiscipline: true}
	cfg.Proxy.ClaudePrompt = &PromptFlags{OutputDiscipline: false}

	flags := cfg.Proxy.EffectivePromptFlags(models.ProfileClaude)
	if !flags.OutputDiscipline {
		t.Error("a zero-value prompt layer must not disable a flag set by the shared layer")
	}
}

// Review finding on YM-136: the deprecated flat fields merge last for Claude and
// are built in code, so they could only ever raise a flag — a named false in
// claude_prompt was undone. The live config sets all flat fields to true.
func TestLos33ClaudePromptBeatsLegacyFlatFields(t *testing.T) {
	cfg := Default()
	cfg.Proxy.PromptToolPrefs = true
	cfg.Proxy.SharedPrompt = &PromptFlags{ToolPrefs: true}
	cfg.Proxy.ClaudePrompt = los33PromptLayer(t, "prompt_tool_prefs: false\nprompt_clarify_first: true\n")

	flags := cfg.Proxy.EffectivePromptFlags(models.ProfileClaude)
	if flags.ToolPrefs {
		t.Error("claude_prompt: prompt_tool_prefs: false must win over the deprecated flat field")
	}
	if !flags.ClarifyFirst {
		t.Error("a flag the legacy layer also enables must still be settable from claude_prompt")
	}
}

// Review finding: decoding the layer into map[string]bool made any non-boolean
// value under a prompt layer a fatal config error, and config.Load answers a
// parse error by replacing the whole file with the defaults.
func TestLos33UnknownNonBoolKeyDoesNotDiscardConfig(t *testing.T) {
	doc := "proxy:\n  token_threshold: 424242\n  shared_prompt:\n    prompt_ungate: true\n    note: free text\n"
	var parsed struct {
		Proxy struct {
			TokenThreshold int          `yaml:"token_threshold"`
			SharedPrompt   *PromptFlags `yaml:"shared_prompt"`
		} `yaml:"proxy"`
	}
	if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("an unknown non-boolean key must not fail the decode: %v", err)
	}
	if parsed.Proxy.TokenThreshold != 424242 {
		t.Errorf("token_threshold = %d, want 424242", parsed.Proxy.TokenThreshold)
	}
	if parsed.Proxy.SharedPrompt == nil || !parsed.Proxy.SharedPrompt.Ungate {
		t.Error("the named boolean key must still be applied")
	}
}

// Review finding: a blank value on a named key used to count as a deliberate
// false. An entry without a value states nothing and must inherit.
func TestLos33BlankValueInherits(t *testing.T) {
	cfg := Default()
	cfg.Proxy.SharedPrompt = &PromptFlags{Rewrite: true}
	cfg.Proxy.OpencodePrompt = los33PromptLayer(t, "prompt_rewrite:\n")

	flags := cfg.Proxy.EffectivePromptFlags(models.ProfileOpencode)
	if !flags.Rewrite {
		t.Error("a key without a value must not disable the inherited flag")
	}
}

// Review finding: a marshal round-trip turned every unnamed key of a layer into
// a written false, which disables it. Only named keys may be written back.
func TestLos33MarshalKeepsOverlaySemantics(t *testing.T) {
	cfg := Default()
	cfg.Proxy.SharedPrompt = &PromptFlags{OutputDiscipline: true, Beweislast: true}
	cfg.Proxy.OpencodePrompt = los33PromptLayer(t, "prompt_clarify_first: true\n")

	raw, err := yaml.Marshal(cfg.Proxy)
	if err != nil {
		t.Fatalf("marshal proxy config: %v", err)
	}
	var back ProxyConfig
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal proxy config: %v", err)
	}

	before := cfg.Proxy.EffectivePromptFlags(models.ProfileOpencode)
	after := back.EffectivePromptFlags(models.ProfileOpencode)
	if before.OutputDiscipline != after.OutputDiscipline || before.ClarifyFirst != after.ClarifyFirst {
		t.Errorf("round-trip changed the effective flags: before=%+v after=%+v", before, after)
	}
	if !after.OutputDiscipline || !after.ClarifyFirst {
		t.Error("round-trip disabled a flag that no layer named")
	}
}

// Review finding: prompt_wiki_first was in the struct but in no merge line, so
// no layer could ever set it. The table-driven merge must carry every field.
func TestLos33EveryFlagFieldIsMergeable(t *testing.T) {
	keys := (&PromptFlags{}).flagFields()
	want := []string{
		"prompt_ungate", "prompt_rewrite", "prompt_enhance", "prompt_tool_prefs",
		"prompt_output_discipline", "prompt_coding_discipline", "prompt_beweislast",
		"prompt_fable", "prompt_scope_discipline", "prompt_delegation_contract",
		"prompt_clarify_first", "prompt_code_tools_first", "prompt_wiki_first",
		"prompt_pattern_suggest",
	}
	for _, key := range want {
		if _, ok := keys[key]; !ok {
			t.Errorf("prompt key %q is missing from the flag table", key)
		}
	}
}

// Review finding: an unknown boolean key in a layer (a typo, or a newer build's
// flag) was recorded as named and then dereferenced against the flag table,
// which panicked `yesmem config` and every save path.
func TestLos33UnknownBoolKeySurvivesMarshal(t *testing.T) {
	cfg := Default()
	cfg.Proxy.ClaudePrompt = los33PromptLayer(t, "prompt_tool_prefs: true\nprompt_tool_pref: true\nenabled: true\n")

	raw, err := yaml.Marshal(cfg.Proxy)
	if err != nil {
		t.Fatalf("marshal must not fail on an unknown key: %v", err)
	}
	var back ProxyConfig
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.EffectivePromptFlags(models.ProfileClaude).ToolPrefs {
		t.Error("the known key next to an unknown one must still apply")
	}
}

// Review finding: a layer built in code was written as all fourteen keys, so a
// reload turned the thirteen unnamed ones into deliberate falses.
func TestLos33ProgrammaticLayerRoundTripKeepsOnlyTrueKeys(t *testing.T) {
	cfg := Default()
	cfg.Proxy.SharedPrompt = &PromptFlags{OutputDiscipline: true}
	cfg.Proxy.ClaudePrompt = &PromptFlags{ToolPrefs: true} // programmatic profile layer

	before := cfg.Proxy.EffectivePromptFlags(models.ProfileClaude)
	raw, err := yaml.Marshal(cfg.Proxy)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ProxyConfig
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	after := back.EffectivePromptFlags(models.ProfileClaude)

	if before.OutputDiscipline != after.OutputDiscipline {
		t.Errorf("round-trip changed a shared flag: before=%v after=%v", before.OutputDiscipline, after.OutputDiscipline)
	}
	if before.ToolPrefs != after.ToolPrefs {
		t.Errorf("round-trip changed the profile flag: before=%v after=%v", before.ToolPrefs, after.ToolPrefs)
	}
}

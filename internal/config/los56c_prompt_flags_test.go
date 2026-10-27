package config

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
	"gopkg.in/yaml.v3"
)

// YM-302(3): feature_defaults carried no assoc_context, so the zero value (off)
// applied to every model whose name matches no model_features prefix — a glm or
// kimi session lost the associative context on the Anthropic path while every
// listed model had it. The fallback has to carry the documented default.
func TestLos56c_FeatureDefaultsEnableAssocContext(t *testing.T) {
	fd := Default().Proxy.FeatureDefaults
	if fd == nil || !fd.AssocContext {
		t.Fatal("feature_defaults does not enable assoc_context: unlisted models lose it")
	}
	tmpl := insertFeatureDefaultsSection("")
	if !containsLine(tmpl, "assoc_context: true") {
		t.Errorf("fresh template still writes assoc_context: false:\n%s", tmpl)
	}
	if gateDefaults["assoc_context"] != "true" {
		t.Errorf("migration writes assoc_context: %s for a missing key", gateDefaults["assoc_context"])
	}
}

// YM-302(4): a rewrite of config.yaml went through yaml.Marshal, and the prompt
// layer's MarshalYAML wrote only the keys this build knows. A key from a newer
// version (or one a user added by hand) was dropped silently. Unknown keys have
// to survive the round trip.
func TestLos56c_PromptFlagsKeepUnknownKeysOnRewrite(t *testing.T) {
	const layer = `prompt_beweislast: true
prompt_new_flag: true
`
	var flags PromptFlags
	if err := yaml.Unmarshal([]byte(layer), &flags); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := yaml.Marshal(&flags)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(out), "prompt_new_flag: true") {
		t.Errorf("unknown key was dropped on rewrite:\n%s", out)
	}
	if !strings.Contains(string(out), "prompt_beweislast: true") {
		t.Errorf("known key was lost on rewrite:\n%s", out)
	}
}

// A resolved layer (no key set) must still write only its true flags, and an
// unknown key must not turn a written false into "off": the overlay semantics
// stay untouched.
func TestLos56c_PromptFlagsOverlaySemanticsUnchanged(t *testing.T) {
	var child PromptFlags
	if err := yaml.Unmarshal([]byte("prompt_fable: false\nprompt_unknown_thing: 7\n"), &child); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var shared PromptFlags
	if err := yaml.Unmarshal([]byte("prompt_fable: true\nprompt_scope_discipline: true\n"), &shared); err != nil {
		t.Fatalf("decode: %v", err)
	}

	merged := &PromptFlags{}
	mergeFlags(merged, &shared)
	mergeFlags(merged, &child)

	if merged.Fable {
		t.Error("a named false did not switch the shared flag off")
	}
	if !merged.ScopeDiscipline {
		t.Error("an unnamed key was treated as switched off")
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(out), "prompt_fable: false") {
		t.Errorf("resolved layer writes unnamed flags:\n%s", out)
	}
	if !strings.Contains(string(out), "prompt_scope_discipline: true") {
		t.Errorf("resolved layer lost its true flag:\n%s", out)
	}
}

// YM-302(1), second round (cold review MEDIUM-5): reading the resolved profile
// instead of the deprecated flat field changes who records REPL shapes. The
// flat field only reaches the Claude profile (claudeLegacyFlags), while the
// fresh template names prompt_pattern_suggest: false in shared_prompt. The
// resolved default is therefore: Claude on, opencode/codex off. That is the
// documented intent of the template, pinned here so a change has to be a
// decision rather than a side effect.
func TestLos56c_PatternSuggestResolutionPerProfile(t *testing.T) {
	p := Default().Proxy
	if !p.EffectivePromptFlags(models.ProfileClaude).PatternSuggest {
		t.Error("the Claude profile lost the pattern recorder")
	}
	for _, profile := range []models.PromptProfile{models.ProfileOpencode, models.ProfileCodex} {
		if p.EffectivePromptFlags(profile).PatternSuggest {
			t.Errorf("profile %v enables the recorder although shared_prompt says false", profile)
		}
	}
}

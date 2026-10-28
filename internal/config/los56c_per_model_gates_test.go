package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// YM-349(5), second round (cold review MEDIUM-3): gateDefaults is one flat map,
// but a fresh install gives each model block its own values. Aligning the flat
// map with feature_defaults fixed one direction and broke the other: a missing
// key in an existing "deepseek" block would now receive plan_checkpoint: true,
// although Default() and the template say false for that model. The value a
// missing key receives has to come from the block it sits in.
func TestLos56c_GateMigrationMatchesPerModelBlocks(t *testing.T) {
	def := Default()
	for _, block := range []string{"claude", "deepseek", "gpt", "openai", "glm", "los56c-custom"} {
		content := "model_features:\n  " + block + ":\n    skill_eval: true\n"
		injected, added := injectMissingGates(content, block+":")
		if added == 0 {
			t.Fatalf("%s: nothing injected, fixture is broken", block)
		}

		var mf struct {
			ModelFeatures map[string]*FeatureGates `yaml:"model_features"`
		}
		if err := yaml.Unmarshal([]byte(injected), &mf); err != nil {
			t.Fatalf("%s: injected text is not YAML: %v\n%s", block, err, injected)
		}
		got := mf.ModelFeatures[block]
		if got == nil {
			t.Fatalf("%s: block missing after injection:\n%s", block, injected)
		}

		want := def.Proxy.ModelFeatures[block]
		if want == nil {
			// A block the defaults do not list falls back to feature_defaults.
			want = def.Proxy.FeatureDefaults
		}
		compareGates(t, "migration/"+block, got, want)
	}
}

// The fresh template has to agree with Default() per block: it is what a new
// install writes, and the migration is supposed to reproduce it for an upgrade.
func TestLos56c_ModelFeaturesTemplateAgreesWithDefault(t *testing.T) {
	var mf struct {
		ModelFeatures map[string]*FeatureGates `yaml:"model_features"`
	}
	if err := yaml.Unmarshal([]byte(modelFeaturesBlock), &mf); err != nil {
		t.Fatalf("template model_features is not YAML: %v\n%s", err, modelFeaturesBlock)
	}
	def := Default()
	for name, want := range def.Proxy.ModelFeatures {
		got := mf.ModelFeatures[name]
		if got == nil {
			t.Errorf("template has no %q block although Default() carries one", name)
			continue
		}
		compareGates(t, "template/"+name, got, want)
	}
}

func compareGates(t *testing.T, label string, got, want *FeatureGates) {
	t.Helper()
	cases := []struct {
		key  string
		got  bool
		want bool
	}{
		{"skill_eval", got.SkillEval, want.SkillEval},
		{"briefing", got.Briefing, want.Briefing},
		{"rules_reminder", got.RulesReminder, want.RulesReminder},
		{"plan_checkpoint", got.PlanCheckpoint, want.PlanCheckpoint},
		{"think_reminder", got.ThinkReminder, want.ThinkReminder},
		{"timestamps", got.Timestamps, want.Timestamps},
		{"assoc_context", got.AssocContext, want.AssocContext},
		{"loop_warning", got.LoopWarning, want.LoopWarning},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: %s = %v, want %v", label, c.key, c.got, c.want)
		}
	}
}

// The template's model_features section is spliced into a config by MigrateConfig;
// it starts with the section header, so it is decodable on its own.
func TestLos56c_TemplateHasNoBlockOutsideTheDefaults(t *testing.T) {
	var mf struct {
		ModelFeatures map[string]*FeatureGates `yaml:"model_features"`
	}
	if err := yaml.Unmarshal([]byte(modelFeaturesBlock), &mf); err != nil {
		t.Fatalf("template model_features is not YAML: %v", err)
	}
	def := Default()
	for name := range mf.ModelFeatures {
		if _, ok := def.Proxy.ModelFeatures[name]; !ok {
			t.Errorf("template carries %q, but Default() has no such block: the two would drift", name)
		}
	}
	if !strings.Contains(modelFeaturesBlock, "model_features:") {
		t.Error("template constant lost its section header")
	}
}

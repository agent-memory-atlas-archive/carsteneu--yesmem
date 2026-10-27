package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// YM-290 (BRF-I2, YM-113): two migration paths disagreed about skill_eval. A
// config without a feature_defaults section got the documented template values
// (skill_eval: true, insertFeatureDefaultsSection), a config whose section only
// misses the key got gateDefaults["skill_eval"] — false. config.Default() and
// the fresh-install template both enable it, so an upgraded install silently
// lost the block a fresh install gets.
func TestLos27b_SkillEvalMigrationDefaultMatchesDefault(t *testing.T) {
	want := strconv.FormatBool(Default().Proxy.FeatureDefaults.SkillEval)
	if got := gateDefaults["skill_eval"]; got != want {
		t.Errorf("gateDefaults[skill_eval] = %q, want %q (config.Default())", got, want)
	}
}

// The fixture is the real shape: feature_defaults and model_features live under
// proxy:, and both sections miss the skill_eval key.
//
// Load() alone cannot prove the injection for feature_defaults: it starts from
// Default() and merges into that pointer, so a missing key would read as true
// there anyway. The raw-text check is what catches "migration stopped writing".
// The model entry is replaced (not merged) by the decoder, so that half
// discriminates on its own.
func TestLos27b_MigrateInjectsSkillEvalTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`proxy:
  enabled: true
  model_features:
    claude:
      briefing: true
      timestamps: false
  feature_defaults:
    briefing: true
    timestamps: false
`), 0644)

	if _, err := MigrateConfig(path); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	if got := strings.Count(string(data), "skill_eval: true"); got != 2 {
		t.Errorf("expected skill_eval: true in both migrated sections, got %d:\n%s", got, data)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if claude := cfg.Proxy.ModelFeatures["claude"]; claude == nil || !claude.SkillEval {
		t.Errorf("migrated model entry has skill_eval off: %+v", claude)
	}
	if defaults := cfg.Proxy.FeatureDefaults; defaults == nil || !defaults.SkillEval {
		t.Errorf("migrated feature_defaults has skill_eval off: %+v", defaults)
	}
}

package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// decodeGates decodes a gate document the way Load does.
func decodeGates(t *testing.T, doc string) ProxyConfig {
	t.Helper()
	var pc ProxyConfig
	if err := yaml.Unmarshal([]byte(doc), &pc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return pc
}

// YM-455: a written false has to outrank feature_defaults, and that includes the
// YAML 1.1 spellings. yaml.v3 resolves yes/no/on/off to a string in an untyped
// decode but writes them into a bool field, so a presence check on the raw type
// would treat "no" as unset and switch the gate back on.
func TestFeatureGatesPresence_YAML11FalseCounts(t *testing.T) {
	pc := decodeGates(t, `
model_features:
  claude:
    loop_warning: no
    timestamps: off
feature_defaults:
  loop_warning: true
  timestamps: true
`)
	claude := pc.ModelFeatures["claude"]
	if claude == nil {
		t.Fatal("claude block missing")
	}
	if claude.LoopWarning {
		t.Error("loop_warning: no decoded to true")
	}
	if !claude.Names("loop_warning") {
		t.Error("loop_warning: no must count as written, otherwise feature_defaults overrides it")
	}
	if !claude.Names("timestamps") {
		t.Error("timestamps: off must count as written")
	}
}

// A key without a value states nothing.
func TestFeatureGatesPresence_NullValueNamesNothing(t *testing.T) {
	pc := decodeGates(t, `
model_features:
  gd-flash:
    loop_warning:
feature_defaults:
  loop_warning: true
`)
	gd := pc.ModelFeatures["gd-flash"]
	if gd == nil {
		t.Fatal("gd-flash block missing")
	}
	if gd.Names("loop_warning") {
		t.Error("an empty value must stay no opinion, so feature_defaults still speaks")
	}
}

// A struct round trip must not turn "unnamed" into "written false": MarshalYAML
// writes only the keys the block named, so the inheritance survives
// MergeDefaults/Save instead of being frozen into the file.
func TestFeatureGatesPresence_RoundTripKeepsInheritance(t *testing.T) {
	pc := decodeGates(t, `
model_features:
  gd-flash:
    briefing: true
feature_defaults:
  briefing: true
  loop_warning: true
`)
	if pc.ModelFeatures["gd-flash"].Names("loop_warning") {
		t.Fatal("fixture is wrong: loop_warning should start unnamed")
	}

	raw, err := yaml.Marshal(pc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ProxyConfig
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if back.ModelFeatures["gd-flash"].Names("loop_warning") {
		t.Errorf("round trip made an unnamed key look written; marshaled form was:\n%s", raw)
	}
	if !back.ModelFeatures["gd-flash"].Names("briefing") {
		t.Error("round trip lost a key the block did name")
	}
	if !back.FeatureDefaults.LoopWarning {
		t.Error("round trip lost feature_defaults.loop_warning")
	}
}

// A gate key of a merged block (<<) is stated by the block, because the decoder
// applies it. Without this the anchor's written false would lose against
// feature_defaults and the gate would switch back on.
func TestFeatureGatesPresence_MergeKeyCountsAsStated(t *testing.T) {
	pc := decodeGates(t, `
model_features:
  gd-flash: &shared
    loop_warning: false
    briefing: true
  gd-pro:
    <<: *shared
feature_defaults:
  loop_warning: true
  briefing: true
`)
	pro := pc.ModelFeatures["gd-pro"]
	if pro == nil {
		t.Fatal("gd-pro block missing")
	}
	if pro.LoopWarning {
		t.Error("the merged loop_warning: false did not reach the struct")
	}
	if !pro.Briefing {
		t.Fatal("fixture is inert: the merge key was not applied at all")
	}
	if !pro.Names("loop_warning") {
		t.Error("a merged gate key must count as stated, otherwise feature_defaults overrides it")
	}
	if !pro.Names("briefing") {
		t.Error("a merged gate key must count as stated")
	}
}

// A key this build does not know must survive a rewrite: dropping it would
// silently delete a setting the user made (YM-302).
func TestFeatureGatesPresence_UnknownKeySurvivesRoundTrip(t *testing.T) {
	pc := decodeGates(t, `
model_features:
  claude:
    briefing: true
    future_gate: true
`)
	raw, err := yaml.Marshal(pc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ProxyConfig
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if !back.ModelFeatures["claude"].Names("future_gate") && !containsKey(raw, "future_gate") {
		t.Errorf("an unknown gate key was dropped by the round trip:\n%s", raw)
	}
}

func containsKey(raw []byte, key string) bool {
	return len(raw) > 0 && strings.Contains(string(raw), key+":")
}

// A block built in code has no key set and states every field, so a marshal
// writes all of them: that is the form Default() hands to a fresh config.
func TestFeatureGatesPresence_CodeBlockWritesEveryField(t *testing.T) {
	g := FeatureGates{Briefing: true}
	raw, err := yaml.Marshal(g)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back FeatureGates
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	for _, key := range []string{"briefing", "timestamps", "loop_warning", "assoc_context", "skill_eval"} {
		if !back.Names(key) {
			t.Errorf("key %q missing from a code-built block after a round trip:\n%s", key, raw)
		}
	}
	if !back.Briefing {
		t.Error("briefing value lost")
	}
}

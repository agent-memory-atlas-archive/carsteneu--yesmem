package proxy

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/carsteneu/yesmem/internal/config"
)

// decodeProxyGates decodes a YAML document the way config.Load does, so the
// presence information of the gate blocks is exercised instead of simulated by
// a struct literal.
func decodeProxyGates(t *testing.T, doc string) *Config {
	t.Helper()
	var pc config.ProxyConfig
	if err := yaml.Unmarshal([]byte(doc), &pc); err != nil {
		t.Fatalf("decode gate document: %v", err)
	}
	return &Config{ModelFeatures: pc.ModelFeatures, FeatureDefaults: pc.FeatureDefaults}
}

// YM-455 (PRX-P05b): a model block only states the keys it names; an unset key
// inherits feature_defaults. Before this, resolveFeatureGate stopped at the
// longest matching block, so every key the block did not name read as the zero
// value and feature_defaults was consulted only when no block matched at all.
// Live consequence: gd-flash, gd-pro and deepseek carried the traffic with
// loop_warning: false although feature_defaults said true, and the loop warning
// never fired (0 "LOOP:" lines at 1916 requests).
func TestYM455_UnsetKeyInheritsFeatureDefaults(t *testing.T) {
	cfg := decodeProxyGates(t, `
model_features:
  gd-flash:
    skill_eval: true
    briefing: true
  deepseek:
    skill_eval: true
feature_defaults:
  skill_eval: true
  briefing: true
  loop_warning: true
`)

	if !isFeatureEnabled(cfg, "gd-flash", "loop_warning") {
		t.Error("gd-flash names no loop_warning, so it must inherit feature_defaults.loop_warning=true")
	}
	if !isFeatureEnabled(cfg, "deepseek-v4-pro", "loop_warning") {
		t.Error("deepseek-v4-pro names no loop_warning, so it must inherit feature_defaults.loop_warning=true")
	}
	if !isFeatureEnabled(cfg, "gd-flash", "briefing") {
		t.Error("a key the block names itself must still come from the block")
	}
	if isFeatureEnabled(cfg, "gd-flash", "timestamps") {
		t.Error("feature_defaults sets no timestamps, so the inherited value stays false")
	}
}

// The other direction of the same rule: an explicitly written false in the
// matching block outranks feature_defaults. This is what makes the presence
// set necessary instead of a plain "true counts" overlay.
func TestYM455_ExplicitFalseInBlockWins(t *testing.T) {
	cfg := decodeProxyGates(t, `
model_features:
  claude:
    loop_warning: false
feature_defaults:
  loop_warning: true
`)

	if isFeatureEnabled(cfg, "claude-opus-4-6", "loop_warning") {
		t.Error("claude names loop_warning: false, so the block wins over feature_defaults")
	}
}

// An empty block states nothing, so it inherits everything.
func TestYM455_EmptyBlockInheritsEverything(t *testing.T) {
	cfg := decodeProxyGates(t, `
model_features:
  glm: {}
feature_defaults:
  briefing: true
  loop_warning: true
`)

	if !isFeatureEnabled(cfg, "glm-5.2", "briefing") {
		t.Error("an empty model block must inherit feature_defaults.briefing")
	}
	if !isFeatureEnabled(cfg, "glm-5.2", "loop_warning") {
		t.Error("an empty model block must inherit feature_defaults.loop_warning")
	}
}

// No block matches: the verdict comes from feature_defaults alone.
func TestYM455_NoBlockMatchesUsesFeatureDefaults(t *testing.T) {
	cfg := decodeProxyGates(t, `
model_features:
  claude:
    loop_warning: false
feature_defaults:
  loop_warning: true
`)

	if !isFeatureEnabled(cfg, "kimi-k2", "loop_warning") {
		t.Error("an unlisted model must read feature_defaults.loop_warning=true")
	}
}

// feature_defaults missing, block key unset: nothing to inherit, false.
func TestYM455_NoFeatureDefaultsStaysFalse(t *testing.T) {
	cfg := decodeProxyGates(t, `
model_features:
  gd-flash:
    briefing: true
`)

	if isFeatureEnabled(cfg, "gd-flash", "loop_warning") {
		t.Error("without feature_defaults an unset key stays false")
	}
	if !isFeatureEnabled(cfg, "gd-flash", "briefing") {
		t.Error("the key the block names must still be read")
	}
}

// A block built in code carries no key set: it cannot tell an unset field from
// a written false, so it states what its fields state. Default() and every
// programmatic config keep the behaviour they had before this change.
func TestYM455_ProgrammaticBlockStatesItsFields(t *testing.T) {
	cfg := &Config{
		ModelFeatures: map[string]*config.FeatureGates{
			"claude": {Briefing: true},
		},
		FeatureDefaults: &config.FeatureGates{Briefing: true, Timestamps: true},
	}

	if isFeatureEnabled(cfg, "claude-opus-4-6", "timestamps") {
		t.Error("a code-built block must not inherit: Default() decides its falses on purpose")
	}
	if !isFeatureEnabled(cfg, "claude-opus-4-6", "briefing") {
		t.Error("a code-built block still reads its own fields")
	}
}

// resolveThinkReminderMinChars has the same shape with an int. Its "no opinion"
// sentinel stays 0: an explicit 0 and an absent key are indistinguishable in the
// file format, and the per-key migration writes 0 into blocks. A decoded block
// without the key therefore keeps inheriting, exactly as before.
func TestYM455_ThinkReminderMinCharsInheritsFromDefaults(t *testing.T) {
	cfg := decodeProxyGates(t, `
model_features:
  deepseek:
    briefing: true
feature_defaults:
  think_reminder_min_chars: 10
`)

	if got := resolveThinkReminderMinChars(cfg, "deepseek-v4-pro"); got != 10 {
		t.Errorf("think_reminder_min_chars = %d, want the inherited 10", got)
	}
}

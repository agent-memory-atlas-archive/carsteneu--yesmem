package setup

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
	"gopkg.in/yaml.v3"
)

// YM-302(3), second round (cold review MEDIUM-4): config.Default() and the
// migration template enable assoc_context, but the template a fresh install
// writes still said false. Because Load() starts from Default() and lets the
// file win, an explicit false switched the feature off on exactly the path a new
// host takes. The generated config has to state what Default() means.
func TestLos56c_GeneratedConfigMatchesFeatureDefaults(t *testing.T) {
	raw := generateConfig("deepseek-v4-flash", true, "", "sk-ds-key", "https://api.deepseek.com/v1",
		"deepseek", "ghostty", "deepseek-v4-flash", "deepseek-v4-pro", "deepseek-v4-flash")

	var parsed struct {
		Proxy config.ProxyConfig `yaml:"proxy"`
	}
	if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("generated config is not YAML: %v", err)
	}
	got := parsed.Proxy.FeatureDefaults
	if got == nil {
		t.Fatal("generated config carries no feature_defaults section")
	}
	want := config.Default().Proxy.FeatureDefaults
	if want == nil {
		t.Fatal("config.Default() carries no FeatureDefaults")
	}

	if got.AssocContext != want.AssocContext {
		t.Errorf("generated feature_defaults: assoc_context=%v, Default() says %v", got.AssocContext, want.AssocContext)
	}
	if got.Briefing != want.Briefing {
		t.Errorf("generated feature_defaults: briefing=%v, Default() says %v", got.Briefing, want.Briefing)
	}
	if got.ThinkReminder != want.ThinkReminder {
		t.Errorf("generated feature_defaults: think_reminder=%v, Default() says %v", got.ThinkReminder, want.ThinkReminder)
	}
	if got.Timestamps != want.Timestamps {
		t.Errorf("generated feature_defaults: timestamps=%v, Default() says %v", got.Timestamps, want.Timestamps)
	}
	if got.LoopWarning != want.LoopWarning {
		t.Errorf("generated feature_defaults: loop_warning=%v, Default() says %v", got.LoopWarning, want.LoopWarning)
	}
}

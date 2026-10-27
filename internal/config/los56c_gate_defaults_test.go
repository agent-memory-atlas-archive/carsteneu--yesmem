package config

import (
	"strconv"
	"strings"
	"testing"
)

// YM-349(5): a missing gate key in an existing config is filled from
// gateDefaults, a fresh install reads the template, and the runtime fallback is
// feature_defaults from config.Default(). When the three disagree, an upgrade
// silently switches a block off that a fresh install has on. The migration
// value is the one that decides for existing users, so it has to follow
// Default().
func TestLos56c_GateDefaultsMatchFeatureDefaults(t *testing.T) {
	fd := Default().Proxy.FeatureDefaults
	if fd == nil {
		t.Fatal("Default() carries no FeatureDefaults")
	}

	want := map[string]string{
		"assoc_context":            strconv.FormatBool(fd.AssocContext),
		"briefing":                 strconv.FormatBool(fd.Briefing),
		"loop_warning":             strconv.FormatBool(fd.LoopWarning),
		"plan_checkpoint":          strconv.FormatBool(fd.PlanCheckpoint),
		"rules_reminder":           strconv.FormatBool(fd.RulesReminder),
		"skill_eval":               strconv.FormatBool(fd.SkillEval),
		"think_reminder":           strconv.FormatBool(fd.ThinkReminder),
		"think_reminder_min_chars": strconv.Itoa(fd.ThinkReminderMinChars),
		"timestamps":               strconv.FormatBool(fd.Timestamps),
	}

	for key, expected := range want {
		got, ok := gateDefaults[key]
		if !ok {
			t.Errorf("gateDefaults has no value for %q", key)
			continue
		}
		if got != expected {
			t.Errorf("gateDefaults[%q] = %s, but config.Default() says %s", key, got, expected)
		}
	}
	if len(gateDefaults) != len(want) {
		t.Errorf("gateDefaults carries %d keys, the test covers %d: a new gate needs a line here", len(gateDefaults), len(want))
	}
}

// The fresh template has to agree as well: it is the value every new install
// writes into config.yaml, and the migration reads the same section.
func TestLos56c_MigrationTemplateAgreesWithFeatureDefaults(t *testing.T) {
	fd := Default().Proxy.FeatureDefaults
	tmpl := insertFeatureDefaultsSection("")

	cases := map[string]bool{
		"briefing: true":        fd.Briefing,
		"plan_checkpoint: true": fd.PlanCheckpoint,
		"rules_reminder: true":  fd.RulesReminder,
		"think_reminder: true":  fd.ThinkReminder,
		"timestamps: true":      fd.Timestamps,
		"assoc_context: true":   fd.AssocContext,
		"loop_warning: true":    fd.LoopWarning,
	}
	for line, on := range cases {
		if !on {
			continue
		}
		if !containsLine(tmpl, line) {
			t.Errorf("fresh template lacks %q although Default() enables it:\n%s", line, tmpl)
		}
	}
}

func containsLine(text, line string) bool {
	for _, got := range splitLines(text) {
		if strings.TrimSpace(got) == line {
			return true
		}
	}
	return false
}

func splitLines(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			out = append(out, text[start:i])
			start = i + 1
		}
	}
	return append(out, text[start:])
}

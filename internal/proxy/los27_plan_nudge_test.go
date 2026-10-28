package proxy

import (
	"strings"
	"testing"
)

// YM-111: the plan nudge was the only proxy injection that was still German,
// while every other injected instruction in the same pipeline is English.
func TestLos27_PlanNudgeIsEnglish(t *testing.T) {
	got := planNudgeText("docs/plan.md")

	if strings.Contains(got, "Du hast einen Implementierungsplan") {
		t.Errorf("plan nudge is still hardcoded German: %q", got)
	}
	if !strings.Contains(got, "docs/plan.md") {
		t.Errorf("plan nudge lost the plan file name: %q", got)
	}
	if !strings.Contains(got, "set_plan()") {
		t.Errorf("plan nudge lost the set_plan() instruction: %q", got)
	}
}

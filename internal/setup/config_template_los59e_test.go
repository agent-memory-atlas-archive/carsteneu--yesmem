package setup

import (
	"strings"
	"testing"
)

// YM-431 finding 3: the template shipped code_nav_dismiss_count, but no reader ever
// consulted it. Since YM-425 one dismiss_code_nav call lifts the block for the
// session, and that threshold is hard-coded (internal/hooks/check.go calls
// store.CodeNavDismissed). The knob therefore promised an effect it could not have.
// The struct field stays, so an existing config.yaml does not turn the key into an
// unknown one (Learning #106662, same move as paths.bleve_index in Los 11l).
func TestConfigTemplateDropsCodeNavDismissCount_Los59e(t *testing.T) {
	cfgStr := generateConfig("deepseek-v4-flash", true, "sk-ant", "", "", "openai_compatible", "ghostty", "narrative", "quality", "summarize")

	if strings.Contains(cfgStr, "code_nav_dismiss_count") {
		t.Errorf("setup template still offers code_nav_dismiss_count, but no production path reads it")
	}
	if !strings.Contains(cfgStr, `code_nav_mode: "block"`) {
		t.Errorf("setup template lost code_nav_mode, which does have a reader")
	}
}

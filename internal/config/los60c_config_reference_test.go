package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-436 finding 2: docs/config-reference.md offered code_nav_dismiss_count with a
// default of 5 and promised "Permanently disable after N dismissals". Both are
// wrong: the threshold is a fixed single dismissal since YM-425, and the key left
// Default() and the setup template with YM-431. The struct field stays only so an
// existing config.yaml does not fail on an unknown key, which means the reference
// must not present the key as configurable, and the tool reference must not promise
// a number of dismissals that nothing reads.
func TestLos60cConfigReferenceKeepsRetiredDismissCountOut(t *testing.T) {
	doc := readRepoDoc(t, "config-reference.md")

	for i, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "| `code_nav_dismiss_count`") {
			t.Errorf("config-reference.md:%d still offers the retired key as a config row: %q", i+1, line)
		}
	}
	if strings.Contains(doc, "Permanently disable after N dismissals") {
		t.Error("config-reference.md still promises a dismiss count that nothing reads")
	}
	if !strings.Contains(doc, "`nudge` (hint only)") {
		t.Error("config-reference.md no longer documents nudge as hint only, which is what the hook does")
	}

	tools := readRepoDoc(t, "mcp-tools-reference.md")
	if strings.Contains(tools, "After 5 dismissals") {
		t.Error("mcp-tools-reference.md still promises five dismissals; one call lifts the block (YM-425)")
	}
}

// readRepoDoc reads a file of the repository's docs directory. The test package sits
// two levels below it, so the path is stable for both go test and a built binary.
func readRepoDoc(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
	if err != nil {
		t.Fatalf("read docs/%s: %v", name, err)
	}
	return string(data)
}

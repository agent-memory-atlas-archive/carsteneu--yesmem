package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// YM-268 [CFG-03] (Los 55c), Befund 6: config.yaml wurde per WriteFile in die
// vorhandene Datei geschrieben. Ein Absturz mitten im Schreiben hinterlaesst
// damit eine halbe Datei, und die traegt die API-Keys. Ein atomarer Write
// ersetzt die Datei per rename, also mit neuer Inode und ohne Zwischenstand.
func TestLos55c_ConfigWriteReplacesTheFileInsteadOfRewritingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := SetValue(path, "proxy.token_threshold", 200000); err != nil {
		t.Fatalf("first write: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first write: %v", err)
	}
	if perm := before.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.yaml mode = %04o, want 0600 (the file carries the API keys)", perm)
	}

	if err := SetValue(path, "proxy.token_threshold", 300000); err != nil {
		t.Fatalf("second write: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after second write: %v", err)
	}
	if os.SameFile(before, after) {
		t.Error("config.yaml was rewritten in place: a crash mid-write leaves a truncated file. An atomic write renames a fresh file over it")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

// Befund 6, zweiter Teil: jede Aenderung legte ein Backup an, ohne je eines zu
// entfernen. Nach einem halben Jahr config set liegt ein Verzeichnis voller
// Kopien der API-Keys.
func TestLos55c_ConfigBackupsStayBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	for i := 1; i <= 13; i++ {
		if err := SetValue(path, "proxy.token_threshold", i*1000); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	backups := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "config.yaml.bak.") {
			backups++
		}
	}
	if backups != 10 {
		t.Errorf("%d config.yaml backups retained, want 10 (the number of writes that must not grow without bound)", backups)
	}
}

// Befund 7: rule_guard.model wird von plugins/opencode-yesmem-v2/rule_guard.ts
// gelesen, stand aber in keinem Go-Schema. Damit war der Key per Hand
// editierbar, aber nicht adressierbar: NormalizeSetPath wies ihn ab, und
// checkKeys fuehrte ihn nur ueber eine Sonderliste als fremd.
func TestLos55c_RuleGuardModelIsPartOfTheSchema(t *testing.T) {
	normalized, err := NormalizeSetPath("rule_guard.model")
	if err != nil {
		t.Fatalf("rule_guard.model is read by the opencode plugin but the Go schema rejects it: %v", err)
	}
	if normalized != "rule_guard.model" {
		t.Errorf("NormalizeSetPath(rule_guard.model) = %q, want the same path", normalized)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SetValue(path, "rule_guard.model", "glm-5.2"); err != nil {
		t.Fatalf("set rule_guard.model: %v", err)
	}
	got, err := GetValue(path, "rule_guard.model")
	if err != nil {
		t.Fatalf("get rule_guard.model: %v", err)
	}
	if fmt.Sprintf("%v", got) != "glm-5.2" {
		t.Errorf("rule_guard.model = %v, want glm-5.2", got)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "rule_guard:") {
		t.Error("the loaded config has no rule_guard section: the loader drops a key the plugin reads")
	}

	var raw map[string]any
	if err := yaml.Unmarshal([]byte("rule_guard:\n  model: glm-5.2\nproxy:\n  enabled: true\n"), &raw); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if issues := checkKeys(raw); len(issues) != 0 {
		t.Errorf("schema reports %v for a documented rule_guard section", issues)
	}
}

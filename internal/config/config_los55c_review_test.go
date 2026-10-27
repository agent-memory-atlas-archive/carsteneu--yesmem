package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Cold review 5.3 finding 1: the re-read must carry the same defaults config.Load
// applies. A config.yaml without a token_thresholds block (any file created by
// `config set` rather than setup) would otherwise drop every built-in threshold
// and silently fall back to the global one.
func TestLos55c_ThresholdReReadKeepsTheBuiltInDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("extraction:\n  model: opus\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	thresholds, err := LoadProxyTokenThresholds(path)
	if err != nil {
		t.Fatalf("LoadProxyTokenThresholds: %v", err)
	}
	want := Default().Proxy.TokenThresholds
	if len(want) == 0 {
		t.Fatal("Default() carries no model thresholds: the fixture proves nothing")
	}
	for model, threshold := range want {
		if thresholds[model] != threshold {
			t.Errorf("threshold for %q = %d, want the default %d", model, thresholds[model], threshold)
		}
	}
}

// The file overlays the defaults, it does not replace them.
func TestLos55c_ThresholdReReadOverlaysTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("proxy:\n  token_thresholds:\n    los55c-overlay: 123000\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	thresholds, err := LoadProxyTokenThresholds(path)
	if err != nil {
		t.Fatalf("LoadProxyTokenThresholds: %v", err)
	}
	if thresholds["los55c-overlay"] != 123000 {
		t.Errorf("los55c-overlay = %d, want 123000", thresholds["los55c-overlay"])
	}
	for model, threshold := range Default().Proxy.TokenThresholds {
		if thresholds[model] != threshold {
			t.Errorf("default %q lost by the overlay: %d, want %d", model, thresholds[model], threshold)
		}
	}
}

// Cold review 5.3 finding 4 / 5.4 LOW: the prune matched any name with the
// prefix, so it deleted an operator's own copy, including a rollback copy made
// before an upgrade. Only timestamped backups belong to the rotation.
func TestLos55c_PruneKeepsOperatorCopies(t *testing.T) {
	dir := t.TempDir()
	operatorCopy := filepath.Join(dir, "config.yaml.bak.before-upgrade")
	if err := os.WriteFile(operatorCopy, []byte("api:\n  api_key: keep\n"), 0o600); err != nil {
		t.Fatalf("write operator copy: %v", err)
	}
	for i := 0; i < 11; i++ {
		name := filepath.Join(dir, "config.yaml.bak.20260101T0000"+twoDigits(i))
		if err := os.WriteFile(name, []byte("old\n"), 0o600); err != nil {
			t.Fatalf("write backup %d: %v", i, err)
		}
	}

	PruneBackups(dir, "config.yaml")

	if _, err := os.Stat(operatorCopy); err != nil {
		t.Errorf("the operator's own copy was pruned: %v", err)
	}
	rotated, err := filepath.Glob(filepath.Join(dir, "config.yaml.bak.20260101T0000*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(rotated) != 10 {
		t.Errorf("%d rotated backups retained, want 10", len(rotated))
	}
}

// Cold review 5.3 finding 5 / 5.4 LOW: a crash between CreateTemp and rename
// leaves a 0600 file with the API keys behind. Nothing collected it. An old temp
// file is garbage; a fresh one belongs to a writer that is still running.
func TestLos55c_PruneReapsStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, configTempPrefix+"stale")
	fresh := filepath.Join(dir, configTempPrefix+"fresh")
	for _, path := range []string{stale, fresh} {
		if err := os.WriteFile(path, []byte("api:\n  api_key: leaked\n"), 0o600); err != nil {
			t.Fatalf("write temp file: %v", err)
		}
	}
	old := time.Now().Add(-2 * configTempMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age the stale temp file: %v", err)
	}

	PruneBackups(dir, "config.yaml")

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("an old temp file carrying the API keys survived the prune")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh temp file was reaped while its writer may still own it: %v", err)
	}
}

// Cold review 5.3 finding 3: a hand-edited rule_guard section must not reject the
// whole file. Before the key entered the schema the loader ignored any shape; a
// Load that falls back to defaults loses the API keys and every path.
func TestLos55c_UnusualRuleGuardShapeKeepsTheRestOfTheFile(t *testing.T) {
	for name, section := range map[string]string{
		"scalar":   "rule_guard: glm-5.2\n",
		"sequence": "rule_guard:\n  - glm-5.2\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := section + "extraction:\n  model: opus\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write config.yaml: %v", err)
			}

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Extraction.Model != "opus" {
				t.Errorf("extraction.model = %q after a %s rule_guard section, want opus: the file fell back to defaults", cfg.Extraction.Model, name)
			}
		})
	}
}

// A scalar section is the model name written without its key; that reading is
// what makes such a file useful instead of merely loadable.
func TestLos55c_ScalarRuleGuardCarriesTheModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("rule_guard: glm-5.2\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RuleGuard.Model != "glm-5.2" {
		t.Errorf("rule_guard.model = %q, want glm-5.2", cfg.RuleGuard.Model)
	}
	if !strings.Contains(cfg.RuleGuard.Model, "glm") {
		t.Error("the scalar section was not read as the model")
	}
}

// twoDigits formats a decimal with two digits for the fixture names.
func twoDigits(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

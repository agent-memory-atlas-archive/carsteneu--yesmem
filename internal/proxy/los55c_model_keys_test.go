package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// YM-268 [CFG-03] (Los 55c), Befund 2: refreshSharedOverrides iterierte nur die
// Modell-Keys aus dem Config-Snapshot vom Prozessstart. Ein Modell, das erst zur
// Laufzeit per set_config token_threshold:<model> gesetzt wurde, blieb damit
// unsichtbar: der Slot lag in proxy_state und in config.yaml, der Proxy las ihn
// nie und wandte weiter den globalen Schwellwert an.
func TestLos55c_ModelOverrideAppliesForModelSetAtRuntime(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("config_override:token_threshold:deepseek", "500000")

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000
	// The startup snapshot does not know the model. The file does, because
	// set_config writes both the file and the runtime slot.
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "config.yaml"),
		[]byte("proxy:\n  token_threshold: 200000\n  token_thresholds:\n    deepseek: 500000\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	s.refreshConfigOverrides("thread-a")

	if got := s.effectiveTokenThreshold("thread-a", "deepseek-v4"); got != 500000 {
		t.Fatalf("threshold for a runtime-added model = %d, want the 500000 stored in proxy_state", got)
	}
}

// The withdrawal of such an override has to reach the proxy too. The key set is
// no longer a startup snapshot, so a model that lost its slot must fall back to
// the value the file still carries for it.
func TestLos55c_ModelOverrideWithdrawalReachesTheProxy(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("config_override:token_threshold:deepseek", "500000")

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "config.yaml"),
		[]byte("proxy:\n  token_thresholds:\n    deepseek: 450000\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	s.refreshConfigOverrides("thread-a")
	if got := s.effectiveTokenThreshold("thread-a", "deepseek-v4"); got != 500000 {
		t.Fatalf("seeded override = %d, want 500000", got)
	}

	d.set("config_override:token_threshold:deepseek", "")
	s.overrideRefreshMu.Lock()
	s.lastOverrideRefresh = time.Time{}
	s.overrideRefreshMu.Unlock()
	s.refreshConfigOverrides("thread-a")

	if got := s.effectiveTokenThreshold("thread-a", "deepseek-v4"); got != 450000 {
		t.Errorf("threshold after the override was withdrawn = %d, want the file value 450000", got)
	}
}

// Cold review finding 2: config.Load never returns an error (it falls back to
// defaults and logs), so a corrupt config.yaml must not be able to replace the
// per-model thresholds the proxy holds. A typo in the file would otherwise drop
// every configured threshold and silently run the 180k default.
func TestLos55c_CorruptConfigKeepsTheModelThresholds(t *testing.T) {
	d := newLos26OverrideDaemon()
	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000
	s.cfg.TokenThresholds = map[string]int{"los55c-model": 777000}
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "config.yaml"), []byte("proxy: [this is not a mapping\n"), 0o600); err != nil {
		t.Fatalf("write corrupt config.yaml: %v", err)
	}

	s.refreshConfigOverrides("thread-a")

	if got := s.effectiveTokenThreshold("thread-a", "los55c-model-v1"); got != 777000 {
		t.Errorf("threshold after an unreadable config.yaml = %d, want the in-memory 777000: a parse error is not evidence that the operator dropped the thresholds", got)
	}
}

// Cold review finding 3: the file decides the key set, but a model that lost
// both its file entry and its runtime slot must lose its override too. It used
// to stay at the last known value, because no refresh visited its key again.
func TestLos55c_ModelRemovedFromFileAndSlotDropsItsOverride(t *testing.T) {
	d := newLos26OverrideDaemon()
	d.set("config_override:token_threshold:los55c-gone", "500000")

	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "config.yaml"),
		[]byte("proxy:\n  token_thresholds:\n    los55c-gone: 450000\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	s.refreshConfigOverrides("thread-a")
	if got := s.effectiveTokenThreshold("thread-a", "los55c-gone-v1"); got != 500000 {
		t.Fatalf("seeded override = %d, want 500000", got)
	}

	// The model disappears from the file and its slot is cleared.
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "config.yaml"), []byte("proxy:\n  token_threshold: 200000\n"), 0o600); err != nil {
		t.Fatalf("rewrite config.yaml: %v", err)
	}
	d.set("config_override:token_threshold:los55c-gone", "")
	s.overrideRefreshMu.Lock()
	s.lastOverrideRefresh = time.Time{}
	s.overrideRefreshMu.Unlock()
	s.refreshConfigOverrides("thread-a")

	if got := s.effectiveTokenThreshold("thread-a", "los55c-gone-v1"); got != 200000 {
		t.Errorf("threshold after the model left file and slot = %d, want the global 200000", got)
	}
}

// Cold review 5.3 finding 2: 0 is the documented reset value of set_config.
// Since the file is re-read, a 0 that landed there must not become the effective
// threshold for that model; the global value applies instead.
func TestLos55c_ZeroThresholdInFileFallsBackToTheGlobal(t *testing.T) {
	d := newLos26OverrideDaemon()
	s := fakeDaemon(t, d.handler)
	s.cfg.TokenThreshold = 200000
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "config.yaml"),
		[]byte("proxy:\n  token_thresholds:\n    los55c-zero: 0\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	s.refreshConfigOverrides("thread-a")

	if got := s.effectiveTokenThreshold("thread-a", "los55c-zero-v1"); got != 200000 {
		t.Errorf("threshold for a model stored as 0 = %d, want the global 200000 (0 is the reset value, not a threshold)", got)
	}
}

package daemon

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// setConfigResponse decodes the JSON body of a set_config/get_config answer.
func setConfigResponse(t *testing.T, resp Response) map[string]string {
	t.Helper()
	if resp.Error != "" {
		t.Fatalf("unexpected error response: %s", resp.Error)
	}
	var out map[string]string
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode response %s: %v", resp.Result, err)
	}
	return out
}

// YM-392 [CFG-04] Teil 3 (Los 56i): set_config spiegelte JEDEN proxy.*-Key nach
// proxy_state. Der laufende Proxy liest dort aber ausschliesslich die
// token_threshold-Familie (refreshConfigOverrides). Fuer alle anderen Keys
// entstand ein Slot ohne Leser, und get_config meldete ihn als wirksamen
// Runtime-Override (source=proxy_state), obwohl die Aenderung erst beim
// naechsten Proxy-Start greift.
func TestLos56i_SetConfigDoesNotMirrorAKeyWithoutRuntimeReader(t *testing.T) {
	h, store := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "proxy.cache_keepalive_min_messages", "value": "25"})
	body := setConfigResponse(t, resp)
	if body["source"] != "config.yaml" {
		t.Errorf("source = %q, want config.yaml: the proxy applies this key at start, not from proxy_state", body["source"])
	}

	dead, err := store.GetProxyState("config_override:cache_keepalive_min_messages")
	if err != nil {
		t.Fatalf("read slot: %v", err)
	}
	if dead != "" {
		t.Errorf("a proxy_state slot %q was written for a key no reader builds", dead)
	}

	// The persistent write must still happen: config.yaml is the store for this key.
	value, err := config.GetValue(filepath.Join(h.dataDir, "config.yaml"), "proxy.cache_keepalive_min_messages")
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if value == nil {
		t.Error("proxy.cache_keepalive_min_messages was not written to config.yaml")
	}
}

// Ein Slot, den eine aeltere Version hinterlassen hat, darf get_config nicht
// mehr als wirksamen Override ausgeben. Der Wert aus config.yaml gilt.
func TestLos56i_GetConfigIgnoresAStaleSlotWithoutRuntimeReader(t *testing.T) {
	h, store := mustHandler(t)

	if err := store.SetProxyState("config_override:cache_keepalive_min_messages", "99"); err != nil {
		t.Fatal(err)
	}
	if err := config.SetValue(filepath.Join(h.dataDir, "config.yaml"), "proxy.cache_keepalive_min_messages", 25); err != nil {
		t.Fatalf("seed config.yaml: %v", err)
	}

	body := setConfigResponse(t, h.handleGetConfig(map[string]any{"key": "proxy.cache_keepalive_min_messages"}))
	if body["source"] == "proxy_state" {
		t.Errorf("get_config reports a runtime override from a slot no reader builds: %v", body)
	}
	if body["value"] != "25" {
		t.Errorf("value = %q, want 25 from config.yaml", body["value"])
	}
}

// Die Keys mit echtem Leser behalten den Runtime-Slot, sonst waere die
// Korrektur eine Regression fuer token_threshold.
func TestLos56i_SetConfigStillMirrorsTheThresholdFamily(t *testing.T) {
	h, store := mustHandler(t)

	setConfigResponse(t, h.handleSetConfig(map[string]any{"key": "token_threshold", "value": "400000"}))
	global, err := store.GetProxyState("config_override:token_threshold")
	if err != nil {
		t.Fatal(err)
	}
	if global != "400000" {
		t.Errorf("global threshold slot = %q, want 400000 (refreshSharedOverrides reads it)", global)
	}

	setConfigResponse(t, h.handleSetConfig(map[string]any{"key": "token_threshold:deepseek", "value": "500000"}))
	model, err := store.GetProxyState("config_override:token_threshold:deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if model != "500000" {
		t.Errorf("per-model threshold slot = %q, want 500000", model)
	}

	body := setConfigResponse(t, h.handleGetConfig(map[string]any{"key": "token_threshold:deepseek"}))
	if body["source"] != "proxy_state" {
		t.Errorf("source = %q, want proxy_state: this slot is read at runtime", body["source"])
	}
}

// Dieselbe Begruendung wie in 55c fuer den Modell-Suffix: ein Session-Override
// auf einem Key ohne Runtime-Leser meldet Erfolg und bewirkt nichts. Er wird
// deshalb abgelehnt, statt still zu verpuffen.
func TestLos56i_SetConfigRejectsSessionOverrideWithoutRuntimeReader(t *testing.T) {
	h, store := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "proxy.effort_floor", "value": "high", "session_id": "ses_los56i"})
	if resp.Error == "" {
		t.Fatal("a session override for a key without a runtime reader was accepted")
	}

	if value, _ := store.GetProxyState("config_override:effort_floor:ses_los56i"); value != "" {
		t.Errorf("an unreadable slot was written: %q", value)
	}

	// The session override that does have a reader keeps working.
	if resp := h.handleSetConfig(map[string]any{"key": "token_threshold", "value": "400000", "session_id": "ses_los56i"}); resp.Error != "" {
		t.Errorf("session threshold override stopped working: %s", resp.Error)
	}
}

// Cold-Review-Befund (Los 56i): set_config lehnt einen Session-Override auf
// einem Key ohne Runtime-Leser ab, get_config schwieg bei demselben Aufruf und
// lieferte den globalen Wert, als waere die Session beruecksichtigt worden. Die
// beiden Antworten muessen denselben Vertrag nennen.
func TestLos56i_GetConfigRejectsASessionIDForAKeyWithoutRuntimeReader(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleGetConfig(map[string]any{"key": "proxy.effort_floor", "session_id": "ses_los56i"})
	if resp.Error == "" {
		t.Fatalf("get_config answered a session-scoped question with the global value: %s", resp.Result)
	}
}

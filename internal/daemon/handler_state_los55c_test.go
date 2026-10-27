package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// YM-268 [CFG-03] (Los 55c), Befund 5: set_config wies einen leeren Wert ab
// ("key and value required"). Der leere Wert ist aber der dokumentierte
// Aus-Zustand mehrerer String-Keys, allen voran proxy.effort_floor: ohne ihn
// laesst sich eine gesetzte Untergrenze nur per Hand in der Datei abschalten.
func TestLos55c_SetConfigAcceptsAnEmptyValue(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "proxy.effort_floor", "value": ""})
	if resp.Error != "" {
		t.Fatalf("empty value is the documented off state, but set_config refused it: %s", resp.Error)
	}

	cfgPath := filepath.Join(h.dataDir, "config.yaml")
	got, err := config.GetValue(cfgPath, "proxy.effort_floor")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got == nil {
		t.Fatal("proxy.effort_floor missing from config.yaml: the empty value was not written")
	}
	if s := fmt.Sprintf("%v", got); s != "" {
		t.Errorf("proxy.effort_floor = %q, want the empty string", s)
	}

	// A missing value is not an empty value: the parameter has to be there.
	resp = h.handleSetConfig(map[string]any{"key": "proxy.effort_floor"})
	if resp.Error == "" {
		t.Error("a request without a value parameter was accepted as an empty value")
	}
}

// Befund 5, Session-Variante: derselbe Aus-Zustand muss auch als Runtime-
// Override einer Session setzbar sein, sonst bleibt eine gesetzte Untergrenze
// fuer diese Session bis zum Ende haengen.
//
// NACHGEZOGEN in Los 56i (YM-392 Teil 3): dieser Test behauptete den Schreib-
// Pfad, nicht die Wirkung. Der Slot config_override:effort_floor:<session> hat
// keinen Leser — refreshConfigOverrides liest ausschliesslich die
// token_threshold-Familie —, der Aufruf meldete also Erfolg und bewirkte
// nichts. set_config lehnt die Kombination jetzt ab; der Test prueft die
// Ablehnung statt der toten Zeile. Der Aus-Zustand selbst bleibt ueber den
// persistenten Pfad erreichbar (TestLos55c_SetConfigAcceptsAnEmptyValue).
func TestLos55c_SetConfigSessionOverrideNeedsARuntimeReader(t *testing.T) {
	h, store := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "proxy.effort_floor", "value": "high", "session_id": "ses_los55c"})
	if resp.Error == "" {
		t.Fatal("a session override for a key without a runtime reader was accepted")
	}
	resp = h.handleSetConfig(map[string]any{"key": "proxy.effort_floor", "value": "", "session_id": "ses_los55c"})
	if resp.Error == "" {
		t.Fatal("clearing a dead session override was accepted")
	}

	value, err := store.GetProxyState("config_override:effort_floor:ses_los55c")
	if err != nil {
		t.Fatalf("read slot: %v", err)
	}
	if value != "" {
		t.Errorf("session slot = %q, want no slot at all: nothing reads it", value)
	}
}

// Befund 3: set_config mit session_id UND Modell-Suffix schrieb
// config_override:token_threshold:<model>:<session>. Der Proxy liest
// ausschliesslich config_override:token_threshold:<threadID>, also war der
// geschriebene Slot für niemanden lesbar: der Aufruf meldete Erfolg und
// bewirkte nichts. Die Kombination wird deshalb abgelehnt, statt still zu
// verpuffen.
func TestLos55c_SetConfigRejectsSessionWithPerModelKey(t *testing.T) {
	h, store := mustHandler(t)

	for _, key := range []string{"token_threshold:deepseek", "proxy.token_thresholds.deepseek"} {
		resp := h.handleSetConfig(map[string]any{"key": key, "value": "500000", "session_id": "ses_los55c"})
		if resp.Error == "" {
			t.Fatalf("%s: session_id combined with a per-model key was accepted, but the proxy never reads that slot", key)
		}
		if value, _ := store.GetProxyState("config_override:token_threshold:deepseek:ses_los55c"); value != "" {
			t.Errorf("%s: an unreadable slot was written: %q", key, value)
		}
	}

	// The two supported forms stay reachable.
	if resp := h.handleSetConfig(map[string]any{"key": "token_threshold:deepseek", "value": "500000"}); resp.Error != "" {
		t.Errorf("per-model override without session_id stopped working: %s", resp.Error)
	}
	if resp := h.handleSetConfig(map[string]any{"key": "token_threshold", "value": "400000", "session_id": "ses_los55c"}); resp.Error != "" {
		t.Errorf("session override without a model suffix stopped working: %s", resp.Error)
	}
}

// Cold review finding 5: the empty value is a value for string fields. For a
// numeric one it must still be refused, otherwise a client that forgets the
// number silently stores a zero.
func TestLos55c_EmptyValueStillRejectedForANumericKey(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "proxy.token_threshold", "value": ""})
	if resp.Error == "" {
		t.Fatal("an empty value was accepted for a numeric key")
	}
}

// Cold review 5.4: the model name comes from the caller and was echoed into the
// error text once unescaped, so a control character reached the client terminal.
func TestLos55c_RejectionErrorEscapesTheModelName(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSetConfig(map[string]any{"key": "token_threshold:deep\nseek", "value": "500000", "session_id": "ses_x"})
	if resp.Error == "" {
		t.Fatal("session_id with a per-model key was accepted")
	}
	if strings.ContainsAny(resp.Error, "\n\r") {
		t.Errorf("the error text carries an unescaped model name: %q", resp.Error)
	}
}

package daemon

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// YM-307 [PLG-F27] Teil 1 (Los 56i): der Fallback auf die zuletzt registrierte
// Session bleibt bestehen — ein Abweisen traefe Aufrufer ohne
// YESMEM_SESSION_ID und braeche mehr, als der Befund wiegt. Er darf aber nicht
// still passieren: die Zeile nennt den geratenen Wert und den fehlenden
// Identitaetstraeger, damit der Fall im Betrieb auffindbar ist.
func TestLos56i_GuessedSessionIsLoggedWithItsMissingIdentity(t *testing.T) {
	h, store := mustHandler(t)
	if err := store.SetProxyState("active_session_opencode", "opencode:ses_guessed"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	got := h.resolveActiveSessionFallback(map[string]any{"_source_agent": "opencode"})
	if got != "opencode:ses_guessed" {
		t.Fatalf("fallback = %q, want the stored opencode session (the fallback must keep working)", got)
	}

	out := buf.String()
	if !strings.Contains(out, "opencode:ses_guessed") {
		t.Errorf("the log does not name the guessed session: %q", out)
	}
	if !strings.Contains(out, "guessed") {
		t.Errorf("the log does not say that the identity was guessed: %q", out)
	}
	if !strings.Contains(out, "_caller_pid") {
		t.Errorf("the log does not name the missing identity carrier: %q", out)
	}

	// A guessed identity repeats on every request; the line is throttled so it
	// does not turn into the loudest thing in the log.
	buf.Reset()
	if got := h.resolveActiveSessionFallback(map[string]any{"_source_agent": "opencode"}); got != "opencode:ses_guessed" {
		t.Fatalf("second fallback = %q", got)
	}
	if buf.Len() != 0 {
		t.Errorf("the same guess was logged again within the window: %q", buf.String())
	}
}

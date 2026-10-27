package daemon

import "testing"

// YM-307 [PLG-F27] Teil 2 (Los 56i): Handle persistierte das Arbeitsverzeichnis
// eines opencode-Aufrufs als proxy_state-Schluessel cwd:<session>. Kein Leser
// baut diesen Schluessel: der Proxy holt den cwd aus dem Request, und die
// Agent-Aufloesung liest _cwd direkt (agentByCallerCWD). Ergebnis war eine
// Zeile pro Session, die nie gelesen und nie aufgeraeumt wird.
func TestLos56i_HandleDoesNotPersistAWorkingDirectoryWithoutAReader(t *testing.T) {
	h, store := mustHandler(t)

	resp := h.Handle(Request{
		Method: "whoami",
		Params: map[string]any{
			"_cwd":          "/home/someone/projects/los56i",
			"_session_id":   "opencode:ses_los56i",
			"_source_agent": "opencode",
			"_caller_pid":   float64(0),
		},
	})
	if resp.Error != "" {
		t.Fatalf("whoami: %s", resp.Error)
	}

	if value, _ := store.GetProxyState("cwd:opencode:ses_los56i"); value != "" {
		t.Errorf("a cwd slot %q was persisted although nothing reads it", value)
	}

	// source_agent has a reader (briefing/generate_full.go) and must stay.
	if value, _ := store.GetProxyState("source_agent:opencode:ses_los56i"); value != "opencode" {
		t.Errorf("source_agent slot = %q, want opencode: the briefing reads it", value)
	}
}

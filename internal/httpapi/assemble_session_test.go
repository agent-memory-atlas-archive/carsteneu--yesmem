package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// assembleBriefingCapture records the params of every RPC the assemble handler
// issues and answers generate_briefing with a minimal briefing.
type assembleBriefingCapture struct {
	byMethod map[string]map[string]any
}

func (h *assembleBriefingCapture) Handle(req RPCRequest) RPCResponse {
	if h.byMethod == nil {
		h.byMethod = make(map[string]map[string]any)
	}
	h.byMethod[req.Method] = req.Params
	if req.Method == "generate_briefing" {
		return RPCResponse{Result: json.RawMessage(`{"text":"briefing","code_map":""}`)}
	}
	return RPCResponse{Result: json.RawMessage(`{}`)}
}

// YM-105: the stateless assemble endpoint knows the session id but did not pass
// it on, so agent and non-Claude detection stayed dead for every assembled
// context.
func TestAssembleForwardsSessionIDToBriefing(t *testing.T) {
	captured := &assembleBriefingCapture{}
	srv := &Server{handler: captured, logger: newTestLogger()}

	body := bytes.NewBufferString(`{"session_id":"opencode:ses-xyz","project":"/tmp/proj","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/assemble", body)
	w := httptest.NewRecorder()
	srv.handleAssemble(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	params := captured.byMethod["generate_briefing"]
	if params == nil {
		t.Fatal("generate_briefing was not called")
	}
	if got, _ := params["session_id"].(string); got != "opencode:ses-xyz" {
		t.Errorf("generate_briefing session_id = %q, want %q", got, "opencode:ses-xyz")
	}
}

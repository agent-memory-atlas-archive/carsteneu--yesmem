package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// YM-275(5): the assemble handler generated a briefing, injected it as a
// role:system message and then ran StripReminders, which drops every role:system
// message. The briefing therefore never reached an assembled request, and the
// Anthropic format does not accept role:system in the messages array anyway.
func TestLos56c_AssembleKeepsTheBriefing(t *testing.T) {
	captured := &assembleBriefingCapture{}
	srv := &Server{handler: captured, logger: newTestLogger()}

	body := bytes.NewBufferString(`{"session_id":"ses_los56c","project":"/tmp/proj","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/assemble", body)
	w := httptest.NewRecorder()
	srv.handleAssemble(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp AssembleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Messages) == 0 {
		t.Fatal("assembled no messages")
	}

	foundBriefing := false
	for _, raw := range resp.Messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			t.Errorf("assembled a role:system message, which the messages array does not accept: %v", msg)
		}
		if strings.Contains(los56cMessageText(msg), "briefing") {
			foundBriefing = true
		}
	}
	if !foundBriefing {
		t.Errorf("the generated briefing is missing from the assembled messages: %v", resp.Messages)
	}
}

// los56cMessageText flattens a message content that is either a string or a
// block array, so an assertion does not depend on which shape the pipeline left.
func los56cMessageText(msg map[string]any) string {
	switch c := msg["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, block := range c {
			if m, ok := block.(map[string]any); ok {
				if text, _ := m["text"].(string); text != "" {
					b.WriteString(text)
					b.WriteString("\n")
				}
			}
		}
		return b.String()
	}
	return ""
}

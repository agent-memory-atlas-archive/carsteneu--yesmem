package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// YM-275(5), second round (cold review MEDIUM-2): prepending the briefing as its
// own message moved the first user turn to index 1. CollapseOldMessages keeps
// messages[0] as a slot and blanks its content, so the briefing was replaced by
// "-" on every request above the budget — the same loss one step later.
func TestLos56c_AssembleCollapseKeepsTheBriefing(t *testing.T) {
	captured := &assembleBriefingCapture{}
	srv := &Server{handler: captured, logger: newTestLogger()}

	var msgs []string
	for i := 0; i < 40; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":"%s"}`, role, strings.Repeat("x", 200)))
	}
	body := bytes.NewBufferString(`{"session_id":"ses_los56c_collapse","project":"/tmp/proj","token_budget":1000,"keep_recent":5,"messages":[` + strings.Join(msgs, ",") + `]}`)
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
	first, _ := resp.Messages[0].(map[string]any)
	if role, _ := first["role"].(string); role != "user" {
		t.Errorf("messages[0] is %q, the array has to start with the first user turn", role)
	}

	found := false
	for _, raw := range resp.Messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if content, _ := msg["content"].(string); strings.Contains(content, "briefing") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the briefing is gone after the collapse (messages[0] = %v)", resp.Messages[0])
	}
}

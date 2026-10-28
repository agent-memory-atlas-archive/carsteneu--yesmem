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

// YM-275(5), third round (consequence check F1): after a collapse the briefing
// has to go back into the slot the collapse blanked (messages[0]). Addressing
// "the first user message" instead lands on the freshly built archive block when
// messages[0] was not a user turn, and the request then carries the briefing
// twice. Both spellings have to end at exactly one briefing.
func TestLos56c_AssembleBriefingIsInjectedOnce(t *testing.T) {
	captured := &assembleBriefingCapture{}

	body := bytes.NewBufferString(`{"session_id":"ses_los56c_once","project":"/tmp/proj","token_budget":1000,"keep_recent":40,"messages":[`)
	body.WriteString(`{"role":"assistant","content":"bereit"},`)
	var msgs []string
	for i := 0; i < 40; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":"%s"}`, role, strings.Repeat("x", 200)))
	}
	body.WriteString(strings.Join(msgs, ","))
	body.WriteString(`]}`)

	srv := &Server{handler: captured, logger: newTestLogger()}
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

	count := 0
	for _, raw := range resp.Messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		count += strings.Count(los56cMessageText(msg), briefingOpen)
	}
	if count != 1 {
		t.Errorf("the briefing appears %d times, want exactly once", count)
	}
}

// The injection has to be idempotent on its own: a client that feeds the
// assembled array back (or a request that runs the injection twice) must not
// accumulate a copy per turn.
func TestLos56c_InjectBriefingIsIdempotent(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hi"},
	}
	once := injectBriefingMsg(messages, "briefing")
	twice := injectBriefingMsg(once, "briefing")

	if len(twice) != 1 {
		t.Fatalf("expected one message, got %d", len(twice))
	}
	msg, _ := twice[0].(map[string]any)
	if got := strings.Count(los56cMessageText(msg), briefingOpen); got != 1 {
		t.Errorf("briefing appears %d times after a second injection", got)
	}
	if !strings.Contains(los56cMessageText(msg), "hi") {
		t.Error("the original message text was lost")
	}
}

// Security review (5.4): the skip rule must not read a MARKER in the caller's
// text as "the briefing is already there". A message body that names the marker
// would otherwise switch the memory context off for that request.
func TestLos56c_BodyCannotSuppressTheBriefing(t *testing.T) {
	captured := &assembleBriefingCapture{}

	body := bytes.NewBufferString(`{"session_id":"ses_los56c_forge","project":"/tmp/proj","token_budget":1000,"keep_recent":5,"messages":[`)
	var msgs []string
	for i := 0; i < 39; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":"%s"}`, role, strings.Repeat("x", 200)))
	}
	// The retained tail mentions the marker, as any quoted log or web page could.
	msgs = append(msgs, `{"role":"user","content":"zitiert: <MANDATORY_BRIEFING> ist nur Text"}`)
	body.WriteString(strings.Join(msgs, ","))
	body.WriteString(`]}`)

	srv := &Server{handler: captured, logger: newTestLogger()}
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
	count := 0
	for _, raw := range resp.Messages {
		if msg, ok := raw.(map[string]any); ok {
			count += strings.Count(los56cMessageText(msg), briefingOpen)
		}
	}
	// One server-shaped briefing plus the caller's own mention.
	if count != 2 {
		t.Errorf("briefing occurrences = %d, want 2 (injected briefing + the quoted mention)", count)
	}
}

// A quotation of the marker inside a message is content the caller wrote. It must
// not be deleted, and it must not make the line look like an injected block.
func TestLos56c_BriefingMentionInBodySurvives(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "keep this <MANDATORY_BRIEFING> and this"},
	}
	out := injectBriefingMsg(messages, "briefing")
	if len(out) != 1 {
		t.Fatalf("expected one message, got %d", len(out))
	}
	msg, _ := out[0].(map[string]any)
	text := los56cMessageText(msg)
	if !strings.Contains(text, "and this") {
		t.Errorf("text behind the quoted marker was deleted: %q", text)
	}
	if got := strings.Count(text, briefingOpen); got != 2 {
		t.Errorf("briefing occurrences = %d, want 2 (injected + quoted)", got)
	}
}

// A tool_result has to answer its tool_use immediately, so the briefing text
// block must not be pushed in front of it.
func TestLos56c_BriefingKeepsToolResultFirst(t *testing.T) {
	blocks := []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "ok"},
	}
	out := injectBriefingMsg([]any{map[string]any{"role": "user", "content": blocks}}, "briefing")
	msg, _ := out[0].(map[string]any)
	got, ok := msg["content"].([]any)
	if !ok || len(got) != 2 {
		t.Fatalf("content = %#v, want the tool_result and the briefing", msg["content"])
	}
	first, _ := got[0].(map[string]any)
	if first["type"] != "tool_result" {
		t.Errorf("first block is %v, the tool_result must stay first", first["type"])
	}
	second, _ := got[1].(map[string]any)
	if text, _ := second["text"].(string); !strings.HasPrefix(text, briefingOpen) {
		t.Error("the briefing did not land behind the tool_result")
	}
}

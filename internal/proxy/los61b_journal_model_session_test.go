package proxy

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// YM-440: the sweep in los60d_journal_id_sink_test.go proves the sink calls
// journalToken; these tests prove the payload cannot escape its line. Both
// OpenAI adapters print the client-supplied model, the Anthropic handler prints
// it in the thinking-normalization line, and openai_handler/responses_handler
// print the session they read from a request header. A value carrying a line
// break and an "upstream 429:" tail used to become a second journal entry, and
// proxy_health counts every line matching that shape as an API error. The failure
// a client could trigger was a false DEGRADED/CRITICAL reading, not a cosmetic
// log blemish.
const (
	los61bForgedModel          = "m\n2026/01/02 03:04:05 [req 9 tid=z] upstream 429: rate limited\x1b[31m"
	los61bForgedAnthropicModel = "claude-opus-4-6\n2026/01/02 03:04:05 [req 9 tid=z] upstream 429: rate limited\x1b[31m"
	// No escape sequence here on purpose: net/http rejects CR, LF, BEL and ESC in
	// header values, so a real client cannot send one. Spaces and brackets are
	// legal, and they are all the in-line prefix forgery needs.
	los61bForgedSession   = "ses_abc] [req 9 tid=forged] upstream 429: rate limited"
	los61bForgedTimestamp = "2026/01/02 03:04:05 "
)

// los61bCapAPIError is the cap's own selection regex (caps/bundled-caps/
// proxy_health/CAP.md, the journalctl grep). Every matching line is counted as an
// API error, so this is the counter the forged payload used to move.
var los61bCapAPIError = regexp.MustCompile(`upstream error:|upstream [45][0-9][0-9]:|ping error tid=|fork .*: API error:`)

type los61bCase struct {
	name string
	path string
	body func(t *testing.T, model string) []byte
	run  func(*Server, http.ResponseWriter, *http.Request)
}

func los61bHandlerCases() []los61bCase {
	return []los61bCase{
		{"openai", "/v1/chat/completions", los61bChatBody, (*Server).handleOpenAICompletions},
		{"responses", "/v1/responses", los61bResponsesBody, (*Server).handleResponses},
	}
}

// TestLos61bForgedModelStaysOnOneJournalLine runs each OpenAI adapter twice, once
// with a plain model and once with the forged one, and compares the number of
// journal lines. The count is what a line counter sees, so a difference means a
// client moved the health counter without reaching any upstream limit.
func TestLos61bForgedModelStaysOnOneJournalLine(t *testing.T) {
	up := los61bUpstream(t)

	for _, tc := range los61bHandlerCases() {
		t.Run(tc.name, func(t *testing.T) {
			clean := los61bRun(t, up, tc, "los61b-clean-model", "")
			hostile := los61bRun(t, up, tc, los61bForgedModel, "")

			los61bAssertSameLineCount(t, clean, hostile)
			if !strings.Contains(hostile, "model=m2026/01/0203:04:05") {
				t.Fatalf("model was dropped instead of sanitized, the line lost its payload: %q", hostile)
			}
			los61bAssertNoCapCount(t, hostile)
			los61bAssertNoEscape(t, hostile)
		})
	}
}

// TestLos61bAnthropicHandlerKeepsForgedModelOnOneLine covers the third route.
// proxy.go's thinking normalization printed the model in an unkeyed slot ("for
// %s"), which the sweep skips by construction, and the gate only needs the model
// to start with a known prefix and the client to send thinking.type "enabled".
func TestLos61bAnthropicHandlerKeepsForgedModelOnOneLine(t *testing.T) {
	up := los61bUpstream(t)

	clean := los61bRunAnthropic(t, up, "claude-opus-4-6")
	hostile := los61bRunAnthropic(t, up, los61bForgedAnthropicModel)

	los61bAssertSameLineCount(t, clean, hostile)
	if !strings.Contains(hostile, "adaptive for claude-opus-4-62026/01/02") {
		t.Fatalf("thinking line did not run or lost its payload: %q", hostile)
	}
	if !strings.Contains(clean, "adaptive for claude-opus-4-6\n") {
		t.Fatalf("clean run did not reach the thinking line: %q", clean)
	}
	los61bAssertNoCapCount(t, hostile)
	los61bAssertNoEscape(t, hostile)
}

// TestLos61bForgedSessionCannotCloseThePrefix covers the header slot. net/http
// rejects CR, LF and ESC in header values, so the session cannot open a line of
// its own, but a space and ']' are legal: they close the "[req N tid=...]" prefix
// and let the rest read as journal decoration.
func TestLos61bForgedSessionCannotCloseThePrefix(t *testing.T) {
	up := los61bUpstream(t)

	for _, tc := range los61bHandlerCases() {
		t.Run(tc.name, func(t *testing.T) {
			out := los61bRun(t, up, tc, "los61b-clean-model", los61bForgedSession)

			if strings.Contains(out, "] [req 9") {
				t.Fatalf("session header closed the journal prefix: %q", out)
			}
			if !strings.Contains(out, "session=ses_abcreq9tidforged") {
				t.Fatalf("session log line vanished instead of being sanitized: %q", out)
			}
			los61bAssertNoCapCount(t, out)
		})
	}
}

// los61bAssertSameLineCount compares the whole capture. Counting lines catches the
// forged entry even when the payload imitates the timestamp decoration exactly, and
// it does so without pinning how many lines the handler logs today.
func los61bAssertSameLineCount(t *testing.T, clean, hostile string) {
	t.Helper()
	cleanLines := los61bLines(clean)
	hostileLines := los61bLines(hostile)
	if len(hostileLines) != len(cleanLines) {
		t.Fatalf("hostile value added %d journal line(s): clean=%d hostile=%d\n%s",
			len(hostileLines)-len(cleanLines), len(cleanLines), len(hostileLines), hostile)
	}
	if !los60dLogLinePrefix.MatchString(cleanLines[0]) {
		t.Fatalf("clean run did not produce a dated journal line: %q", clean)
	}
}

// los61bAssertNoCapCount is the claim in the ticket's own terms: no line of the
// capture may trip the cap's selection, and none may start with the forged date.
func los61bAssertNoCapCount(t *testing.T, out string) {
	t.Helper()
	for _, line := range los61bLines(out) {
		if los61bCapAPIError.MatchString(line) {
			t.Fatalf("a forged line would be counted as an API error: %q", line)
		}
		if strings.HasPrefix(line, los61bForgedTimestamp) {
			t.Fatalf("forged journal entry survived: %q", line)
		}
	}
}

// los61bAssertNoEscape is separate because only the body payloads can carry one:
// a request header cannot.
func los61bAssertNoEscape(t *testing.T, out string) {
	t.Helper()
	for _, line := range los61bLines(out) {
		// The adapters colour their own lines, so only the payload's sequence
		// counts here.
		if strings.Contains(line, "\x1b[31m") {
			t.Fatalf("payload kept its escape sequence: %q", line)
		}
	}
}

func los61bLines(out string) []string {
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// los61bSyncBuffer is written by the handler's background goroutines (the daemon
// calls are fired with "go") and read by the test, so it carries its own lock.
// Without it the line-count comparison races as soon as a fixture produces a
// daemon warning, which is exactly the kind of flake the count must not have.
type los61bSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *los61bSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *los61bSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func los61bRun(t *testing.T, up *httptest.Server, tc los61bCase, model, session string) string {
	t.Helper()

	var buf los61bSyncBuffer
	s := fakeDaemon(t, func(string, map[string]any) string { return "null" })
	s.cfg.TargetURL = up.URL
	s.cfg.OpenAITargetURL = up.URL
	s.httpClient = http.DefaultClient
	s.logger = log.New(&buf, "", log.LstdFlags)
	s.timestampStore = NewTimestampStore()
	s.channelLoader = func(string, string) dialogResult { return dialogResult{} }

	if session != "" {
		prevPeer := peerSessionPIDFn
		peerSessionPIDFn = func(string, string) int { return 0 }
		t.Cleanup(func() { peerSessionPIDFn = prevPeer })
	}

	req := httptest.NewRequest("POST", tc.path, bytes.NewReader(tc.body(t, model)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-123")
	if session != "" {
		req.Header.Set("x-opencode-session", session)
	}

	tc.run(s, httptest.NewRecorder(), req)
	return buf.String()
}

// los61bRunAnthropic drives the /v1/messages handler, whose server needs the full
// map set. los26aHandlerServer leaves DataDir empty on purpose, so the daemon
// lookups fail fast instead of dialling a socket.
func los61bRunAnthropic(t *testing.T, up *httptest.Server, model string) string {
	t.Helper()

	var buf los61bSyncBuffer
	s := los26aHandlerServer(t, up.URL)
	s.logger = log.New(&buf, "", log.LstdFlags)

	body, err := json.Marshal(map[string]any{
		"model":      model,
		"stream":     false,
		"max_tokens": 16,
		"thinking":   map[string]any{"type": "enabled"},
		"system": []any{
			map[string]any{"type": "text", "text": "Primary working directory: /home/tester/memory/yesmem"},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "start the task"},
			map[string]any{"role": "assistant", "content": "on it"},
			map[string]any{"role": "user", "content": "check the prompt profile routing"},
		},
	})
	if err != nil {
		t.Fatalf("marshal messages body: %v", err)
	}

	s.handleMessages(httptest.NewRecorder(), los33MessagesRequest(string(body), nil))
	return buf.String()
}

func los61bChatBody(t *testing.T, model string) []byte {
	t.Helper()
	body, err := json.Marshal(OpenAIChatRequest{
		Model:    model,
		Messages: []OpenAIMessage{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatalf("marshal chat body: %v", err)
	}
	return body
}

func los61bResponsesBody(t *testing.T, model string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":             model,
		"input":             "go",
		"max_output_tokens": 64,
	})
	if err != nil {
		t.Fatalf("marshal responses body: %v", err)
	}
	return body
}

// los61bUpstream answers every adapter path with a fixed completion. It is the
// only upstream the handlers see: no provider route is configured on these
// servers, so every model falls back to cfg.TargetURL.
func los61bUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "responses"):
			json.NewEncoder(w).Encode(ResponsesResponse{
				ID:     "resp_1",
				Object: "response",
				Model:  "los61b-clean-model",
				Status: "completed",
				Output: []ResponsesOutputItem{{
					Type:    "message",
					Role:    "assistant",
					Content: []ResponsesContent{{Type: "output_text", Text: "ok"}},
				}},
			})
		case strings.Contains(r.URL.Path, "messages"):
			json.NewEncoder(w).Encode(map[string]any{
				"id":          "msg_1",
				"type":        "message",
				"role":        "assistant",
				"model":       "claude-opus-4-6",
				"content":     []any{map[string]any{"type": "text", "text": "ok"}},
				"stop_reason": "end_turn",
				"usage":       map[string]any{"input_tokens": 10, "output_tokens": 2},
			})
		default:
			json.NewEncoder(w).Encode(OpenAIChatResponse{
				ID:     "chatcmpl-1",
				Object: "chat.completion",
				Model:  "los61b-clean-model",
				Choices: []OpenAIChoice{{
					Index:        0,
					Message:      OpenAIMessage{Role: "assistant", Content: "ok"},
					FinishReason: "stop",
				}},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// YM-249: the codex branch resolved the session from the peer process and set
// the mailbox, but never told the daemon which agent it is. Only the opencode
// branch called register_pid with source_agent, so a codex session stayed
// "claude" for every daemon-side reader (agent rows, briefing profile). Both
// OpenAI handlers carry the same wiring and both have to report it.
func TestCodexRequestsRegisterTheirSourceAgent(t *testing.T) {
	requireProc(t)
	openRollout(t, codexUUIDForTest)

	cases := []struct {
		name string
		path string
		body string
	}{
		{
			name: "responses",
			path: "/v1/responses",
			body: `{"model":"gpt-5.5","input":"hello","stream":false}`,
		},
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":false}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"id": "x", "object": "response", "model": "gpt-5.5", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
					"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
				})
			}))
			defer upstream.Close()

			var mu sync.Mutex
			var registers []map[string]any
			s := fakeDaemon(t, func(method string, params map[string]any) string {
				if method == "register_pid" {
					mu.Lock()
					registers = append(registers, params)
					mu.Unlock()
				}
				return "null"
			})
			s.cfg.TargetURL = upstream.URL
			s.cfg.OpenAITargetURL = upstream.URL
			s.httpClient = http.DefaultClient
			s.timestampStore = NewTimestampStore()

			prevPeer := peerSessionPIDFn
			peerSessionPIDFn = func(string, string) int { return os.Getpid() }
			t.Cleanup(func() { peerSessionPIDFn = prevPeer })

			req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "codex-tui/0.153.4 (linux; x86_64)")

			if c.path == "/v1/responses" {
				s.handleResponses(httptest.NewRecorder(), req)
			} else {
				s.handleOpenAICompletions(httptest.NewRecorder(), req)
			}

			mu.Lock()
			defer mu.Unlock()
			wantSession := "codex:" + codexUUIDForTest
			if len(registers) != 1 {
				t.Fatalf("register_pid calls = %d (%v), want exactly one with the codex identity", len(registers), registers)
			}
			got := registers[0]
			if got["session_id"] != wantSession {
				t.Errorf("register_pid session_id = %v, want %q", got["session_id"], wantSession)
			}
			if got["source_agent"] != "codex" {
				t.Errorf("register_pid source_agent = %v, want codex", got["source_agent"])
			}
			if pid, _ := got["pid"].(float64); int(pid) != os.Getpid() {
				t.Errorf("register_pid pid = %v, want %d", got["pid"], os.Getpid())
			}
		})
	}
}

package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- YM-323 (1): subagent precedence, OpenAI path vs Anthropic path ---

// A positive subagent marker must outrank the cli main-session marker on BOTH
// paths. The Anthropic path already lets the Haiku marker win; a divergence
// means the same client is a subagent on one endpoint and a main session on the
// other, with briefing, stubs, collapse and the emergency brake following one
// verdict and the fork/passthrough the other.
func TestLos26d_SubagentPrecedenceAgreesAcrossPaths(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hello"}}
	cliSystem := []any{map[string]any{"type": "text", "text": "cc_entrypoint=cli"}}

	haikuCLI := map[string]any{"model": "claude-haiku-4-5-20251001", "system": cliSystem}
	if !isSubagentRequest(http.Header{}, msgs, haikuCLI) {
		t.Fatal("anthropic path: haiku+cli is not a subagent, so this test cannot pin agreement")
	}
	if !isOpenAIProxySubagent(http.Header{}, msgs, haikuCLI) {
		t.Error("openai path: haiku+cli is not a subagent, the cli marker outranked the haiku marker")
	}

	plainCLI := map[string]any{"model": "gpt-5.4", "system": cliSystem}
	if isSubagentRequest(http.Header{}, msgs, plainCLI) {
		t.Fatal("anthropic path: cli without a subagent marker was judged a subagent")
	}
	if isOpenAIProxySubagent(http.Header{}, msgs, plainCLI) {
		t.Error("openai path: cli without a subagent marker was judged a subagent")
	}

	// The third declared marker: Claude Code puts its agent type into
	// metadata.user_id, and the OpenAI wire passes metadata through translation.
	declaredAgent := map[string]any{
		"model":    "gpt-5.4",
		"system":   cliSystem,
		"metadata": map[string]any{"user_id": `{"agent_type":"subagent"}`},
	}
	if !isSubagentRequest(http.Header{}, msgs, declaredAgent) {
		t.Fatal("anthropic path: declared agent_type=subagent is not a subagent")
	}
	if !isOpenAIProxySubagent(http.Header{}, msgs, declaredAgent) {
		t.Error("openai path: declared agent_type=subagent is not a subagent")
	}
}

// --- YM-323 (3): system as a plain string ---

// The system prompt arrives as a list from Claude Code and as a plain string
// from other clients. Both forms carry the entrypoint marker; reading only the
// list form dropped the main-session marker and fell through to the thinking
// heuristic, which turns a main session without extended thinking into a
// subagent.
func TestLos26d_MainSessionMarkerFromStringSystem(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hello"}}
	req := map[string]any{
		"model":  "claude-sonnet-5",
		"system": "You are a coding agent. cc_entrypoint=cli",
	}

	if !hasMainSessionMarker(http.Header{}, req) {
		t.Error("hasMainSessionMarker did not read the string form of system")
	}
	if isSubagentRequest(http.Header{}, msgs, req) {
		t.Error("main session with a string system was treated as a subagent")
	}
}

// The same asymmetry exists in the other direction: an SDK subagent that sends
// its entrypoint as a string must still be recognised as a subagent.
func TestLos26d_SubagentMarkerFromStringSystem(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hello"}}
	req := map[string]any{
		"model":  "claude-sonnet-5",
		"system": "cc_entrypoint=sdk-ts",
	}

	if !hasSubagentClientMarker(req) {
		t.Error("hasSubagentClientMarker did not read the string form of system")
	}
	if !isSubagentRequest(http.Header{}, msgs, req) {
		t.Error("SDK subagent with a string system was not treated as a subagent")
	}
}

// --- YM-323 (2): OpenAI paths never reach the thread-state prune ---

const (
	los26dIdleThread = "los26d-idle-thread"
	los26dIdleStub   = "los26d-idle-stub"
	los26dWarmThread = "los26d-warm-thread"
	los26dWarmStub   = "los26d-warm-stub"
)

// los26dPruneFixture builds a server whose next request carries the given
// index, so a caller can put it on the 50th (a prune tick) or just before it.
// It is seeded with one thread and one frozen stub that are three hours old,
// well past the sawtooth state TTL and the stub eviction TTL, plus one warm
// thread and stub that a prune must never touch.
func los26dPruneFixture(t *testing.T, nextReqIdx int64) (*Server, *SawtoothTrigger, *FrozenStubs) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			json.NewEncoder(w).Encode(ResponsesResponse{
				ID:     "resp_123",
				Object: "response",
				Model:  "gpt-5.4",
				Status: "completed",
				Output: []ResponsesOutputItem{{
					Type:    "message",
					Role:    "assistant",
					Content: []ResponsesContent{{Type: "output_text", Text: "ok"}},
				}},
				Usage: &ResponsesUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
			})
		case "/v1/messages":
			json.NewEncoder(w).Encode(map[string]any{
				"id":          "msg_1",
				"type":        "message",
				"role":        "assistant",
				"model":       "claude-sonnet-5",
				"content":     []any{map[string]any{"type": "text", "text": "ok"}},
				"stop_reason": "end_turn",
				"usage":       map[string]any{"input_tokens": 10, "output_tokens": 2},
			})
		default:
			json.NewEncoder(w).Encode(OpenAIChatResponse{
				ID:     "chatcmpl-123",
				Object: "chat.completion",
				Model:  "gpt-5.4",
				Choices: []OpenAIChoice{{
					Index:        0,
					Message:      OpenAIMessage{Role: "assistant", Content: "ok"},
					FinishReason: "stop",
				}},
				Usage: &OpenAIUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
			})
		}
	}))
	t.Cleanup(upstream.Close)

	// The full handler server: handleMessages reaches further than the OpenAI
	// handlers and needs the complete map set. Its cfg.DataDir stays empty, so
	// queryDaemon fails fast instead of dialling a live daemon.
	s := los26aHandlerServer(t, upstream.URL)
	s.cfg.SawtoothEnabled = true
	s.requestIdx.Store(nextReqIdx - 1)

	stale := time.Now().Add(-3 * time.Hour)
	now := time.Now()
	st := s.sawtoothTrigger
	st.mu.Lock()
	st.lastTouch[los26dIdleThread] = stale
	st.lastTotalTokens[los26dIdleThread] = 1234
	st.lastTouch[los26dWarmThread] = now
	st.lastTotalTokens[los26dWarmThread] = 1234
	st.mu.Unlock()

	f := s.frozenStubs
	f.mu.Lock()
	f.lastAccess[los26dIdleStub] = stale
	f.messages[los26dIdleStub] = []any{map[string]any{"role": "user", "content": "old"}}
	f.lastAccess[los26dWarmStub] = now
	f.messages[los26dWarmStub] = []any{map[string]any{"role": "user", "content": "fresh"}}
	f.mu.Unlock()

	return s, st, f
}

func TestLos26d_OpenAIPathsPruneThreadState(t *testing.T) {
	chatBody, _ := json.Marshal(OpenAIChatRequest{
		Model:    "gpt-5.4",
		Messages: []OpenAIMessage{{Role: "user", Content: "Hello"}},
	})
	responsesBody, _ := json.Marshal(map[string]any{"model": "gpt-5.4", "input": "Hello"})
	messagesBody, _ := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "Hello"}},
	})

	cases := []struct {
		name string
		path string
		body []byte
		run  func(*Server, http.ResponseWriter, *http.Request)
	}{
		{"chat/completions", "/v1/chat/completions", chatBody, (*Server).handleOpenAICompletions},
		{"responses", "/v1/responses", responsesBody, (*Server).handleResponses},
		// The Anthropic path had the prune before the shared helper existed;
		// this case pins that moving it out of handleMessages kept the call.
		{"messages", "/v1/messages", messagesBody, (*Server).handleMessages},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, st, f := los26dPruneFixture(t, 50)

			req := httptest.NewRequest("POST", tc.path, bytes.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-test-123")
			w := httptest.NewRecorder()
			tc.run(s, w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}

			if los26dHasThread(st, los26dIdleThread) {
				t.Error("sawtooth state of a three-hour-idle thread survived the prune tick")
			}
			if los26dHasStub(f, los26dIdleStub) {
				t.Error("stale frozen stub survived the prune tick")
			}
			if !los26dHasThread(st, los26dWarmThread) {
				t.Error("the prune tick evicted a warm thread")
			}
			if !los26dHasStub(f, los26dWarmStub) {
				t.Error("the prune tick evicted a warm frozen stub")
			}
		})
	}
}

// The prune is throttled to every 50th request. Without the gate it would walk
// both maps on every request of the hottest paths, so the gate needs its own
// negative case: one request before the tick must leave the stale state alone.
func TestLos26d_PruneRunsOnlyOnTheTick(t *testing.T) {
	chatBody, _ := json.Marshal(OpenAIChatRequest{
		Model:    "gpt-5.4",
		Messages: []OpenAIMessage{{Role: "user", Content: "Hello"}},
	})

	s, st, f := los26dPruneFixture(t, 49)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-123")
	w := httptest.NewRecorder()
	s.handleOpenAICompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !los26dHasThread(st, los26dIdleThread) {
		t.Error("request 49 pruned thread state, the interval is not honoured")
	}
	if !los26dHasStub(f, los26dIdleStub) {
		t.Error("request 49 evicted a frozen stub, the interval is not honoured")
	}
}

func los26dHasThread(st *SawtoothTrigger, threadID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.lastTouch[threadID]
	return ok
}

func los26dHasStub(f *FrozenStubs, threadID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.lastAccess[threadID]
	return ok
}

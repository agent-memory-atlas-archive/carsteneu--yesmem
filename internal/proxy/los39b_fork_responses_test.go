package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// YM-262 Befund 1: /v1/responses teilt den OpenAI-Forward-Pfad, sein Body traegt
// aber "input" statt "messages". Der Fork klonte diesen Body, scheiterte am
// Builder und feuerte dort nie. Der Fix uebersetzt den Body nach
// Chat-Completions, damit der Fork in der Wire-Familie des Clients bleibt: nur so
// passen Endpoint, Body und Credentials zusammen.
func TestResponsesPath_FiresForkInOpenAIFamily(t *testing.T) {
	forkBody := make(chan string, 1)
	forkPath := make(chan string, 1)
	forkAuth := make(chan string, 1)

	// Separate targets: a fork that lands on the Anthropic target is the bug this
	// test is supposed to catch.
	anthropicTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fork must not go to the Anthropic target, got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer anthropicTarget.Close()

	openaiTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":{"type":"text_delta","text":"ok"}}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"input_tokens":50,"output_tokens":10,"input_tokens_details":{"cached_tokens":950}}}}`)
			flusher.Flush()
		case "/v1/chat/completions":
			b, _ := io.ReadAll(r.Body)
			select {
			case forkBody <- string(b):
			default:
			}
			select {
			case forkPath <- r.URL.Path:
			default:
			}
			select {
			case forkAuth <- r.Header.Get("Authorization"):
			default:
			}
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
	}))
	defer openaiTarget.Close()

	s := &Server{
		cfg: Config{
			TargetURL:       anthropicTarget.URL,
			OpenAITargetURL: openaiTarget.URL,
		},
		httpClient: openaiTarget.Client(),
		logger:     log.New(io.Discard, "", 0),
		forkState:  NewForkState(0, 0, 3, 10),
	}
	// The OpenAI fork path pauses for DeepSeek's disk cache; that wait is not
	// what this test is about.
	oldDelay := openAIForkDelay
	openAIForkDelay = 0
	t.Cleanup(func() { openAIForkDelay = oldDelay })
	forked := make(chan struct{}, 1)
	s.forkConfigs = []ForkConfig{{
		Name:      "los39b_responses",
		MaxTokens: 64,
		Prompt:    func(ForkContext) string { return "[FORK]" },
		ParseResult: func(ForkResponse, *Server) error {
			select {
			case forked <- struct{}{}:
			default:
			}
			return nil
		},
	}}

	// The gate needs a proven cache (two warm requests) and token growth.
	threadID := "opencode:ses_los39bfork"
	s.forkState.MarkCacheWarm(threadID)
	s.forkState.MarkCacheWarm(threadID)
	s.forkState.ForceNextFork(threadID)

	reqBody, _ := json.Marshal(map[string]any{
		"model":             "gpt-5.4",
		"instructions":      "Be helpful",
		"input":             "Hello",
		"stream":            true,
		"max_output_tokens": 128,
	})
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-openai")
	req.Header.Set("x-opencode-session", "ses_los39bfork")
	w := httptest.NewRecorder()

	s.handleResponses(w, req)

	select {
	case <-forked:
	case <-time.After(5 * time.Second):
		t.Fatalf("no fork fired for /v1/responses (status=%d body=%s)", w.Code, w.Body.String())
	}

	select {
	case path := <-forkPath:
		if path != "/v1/chat/completions" {
			t.Errorf("fork path = %q, want the OpenAI-compatible endpoint", path)
		}
	default:
		t.Fatal("fork reported success but sent no request upstream")
	}

	select {
	case auth := <-forkAuth:
		if auth != "Bearer sk-test-openai" {
			t.Errorf("fork Authorization = %q, want the client's own credential", auth)
		}
	default:
		t.Fatal("fork request carried no Authorization header")
	}

	select {
	case body := <-forkBody:
		if !strings.Contains(body, `"messages"`) {
			t.Errorf("fork body has no messages array: %s", body)
		}
		if strings.Contains(body, `"input"`) {
			t.Errorf("fork body still carries Responses fields: %s", body)
		}
		if !strings.Contains(body, "[FORK]") {
			t.Errorf("fork body does not carry the fork prompt: %s", body)
		}
	default:
		t.Fatal("no fork body captured")
	}
}

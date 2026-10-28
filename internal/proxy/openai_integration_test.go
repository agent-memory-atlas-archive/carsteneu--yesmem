package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAIIntegration_StreamingRoundTrip(t *testing.T) {
	// Mock OpenAI API that returns OpenAI SSE format
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["messages"]; !ok {
			t.Error("expected messages in request")
		}

		// Return OpenAI SSE stream (passthrough — no translation needed)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		chunks := []string{
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"World!"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	// Find a free port
	addr := freePort(t)

	// Start proxy in background; it ends with the test (YM-399)
	cfg := Config{
		ListenAddr:            addr,
		TargetURL:             upstream.URL,
		OpenAITargetURL:       upstream.URL,
		TokenThreshold:        250000,
		TokenMinimumThreshold: 100000,
		KeepRecent:            10,
		DataDir:               t.TempDir(),
	}
	startProxyForTest(t, cfg)

	// Send OpenAI streaming request
	oaiReq := OpenAIChatRequest{
		Model: "gpt-5.4",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hello"},
		},
		Stream: true,
	}
	body, _ := json.Marshal(oaiReq)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post("http://"+addr+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Parse SSE stream — should be OpenAI format passthrough
	scanner := bufio.NewScanner(resp.Body)
	var gotContent bool
	var gotDone bool
	var contentParts []string
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			gotDone = true
			continue
		}
		var chunk OpenAIStreamChunk
		if json.Unmarshal([]byte(data), &chunk) == nil {
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				gotContent = true
				contentParts = append(contentParts, chunk.Choices[0].Delta.Content)
			}
		}
	}

	if !gotContent {
		t.Error("no content received in stream")
	}
	if !gotDone {
		t.Error("no [DONE] received")
	}

	fullContent := strings.Join(contentParts, "")
	if fullContent != "Hello World!" {
		t.Errorf("content = %q, want 'Hello World!'", fullContent)
	}
}

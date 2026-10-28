package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenAIPath_DumpsRequestBodyWhenDebug verifies that the OpenAI forward path
// (forwardOpenAIWithTracking) writes a request-body dump file when
// YESMEM_PROXY_DEBUG=1, mirroring the Anthropic path (forwardWithAnnotation at
// proxy_forward.go:32). Regression: the OpenAI path was missing the dump call,
// so OpenAI/DeepSeek traffic had no request-body observability.
func TestOpenAIPath_DumpsRequestBodyWhenDebug(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	addr := freePort(t)

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	t.Setenv("YESMEM_PROXY_DEBUG", "1")

	cfg := Config{
		ListenAddr:            addr,
		TargetURL:             upstream.URL,
		OpenAITargetURL:       upstream.URL,
		TokenThreshold:        250000,
		TokenMinimumThreshold: 100000,
		KeepRecent:            10,
		DataDir:               dataDir,
	}
	startProxyForTest(t, cfg)

	oaiReq := OpenAIChatRequest{
		Model: "gpt-5.4",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "dump-me"},
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

	matches, err := filepath.Glob(filepath.Join(dataDir, "debug", "req_*_body.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("OpenAI path did not dump request body: no req_*_body.json in debug/ (expected writeDebugDump in forwardOpenAIWithTracking)")
	}
}

package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// los39DeflationSSE is a minimal Anthropic SSE stream whose message_start carries
// real usage numbers. Deflation only rewrites this event.
const los39DeflationSSE = `data: {"type":"message_start","message":{"usage":{"input_tokens":1000,"cache_read_input_tokens":400,"cache_creation_input_tokens":100}}}` + "\n\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// los39DeflationServer returns a Server whose upstream echoes los39DeflationSSE.
func los39DeflationServer(t *testing.T) *Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, los39DeflationSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)
	dataDir := t.TempDir()
	// The writer owns a 1s ticker; leaving it running outlives the test and
	// allocates inside every later AllocsPerRun window (YM-396).
	cacheStatusWriter := NewCacheStatusWriter(dataDir, "ephemeral", 250000, 100000)
	t.Cleanup(cacheStatusWriter.Stop)
	return &Server{
		cfg: Config{
			TargetURL:            upstream.URL,
			UsageDeflationFactor: 0.7,
		},
		httpClient:        upstream.Client(),
		logger:            log.New(io.Discard, "", 0),
		cacheStatusWriter: cacheStatusWriter,
		forkState:         NewForkState(10000, 60000, 3, 10),
	}
}

// los39RunAnthropicForward sends one /v1/messages request through the Anthropic
// SSE relay and returns the bytes the client received.
func los39RunAnthropicForward(t *testing.T, s *Server, headers map[string]string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":16}`))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	// threadID "" keeps the relay off the daemon (no usage tracking, no sawtooth).
	s.forwardWithAnnotation(rec, req, []byte(`{"model":"claude-opus-5-5","max_tokens":16}`), 1, nil, "los39-deflate", t.TempDir(), "", 1, false)
	return rec.Body.String()
}

func los39InputTokens(t *testing.T, sse string) string {
	t.Helper()
	for _, line := range strings.Split(sse, "\n") {
		if !strings.Contains(line, `"message_start"`) {
			continue
		}
		idx := strings.Index(line, `"input_tokens":`)
		if idx < 0 {
			continue
		}
		rest := line[idx+len(`"input_tokens":`):]
		end := strings.IndexAny(rest, ",}")
		if end < 0 {
			continue
		}
		return rest[:end]
	}
	t.Fatalf("no message_start with input_tokens in client stream:\n%s", sse)
	return ""
}

// YM-142: usage deflation belongs to Claude Code only. It exists to suppress the
// "Context low" warning in CC, which manages no context of its own. Every other
// Anthropic-path client (opencode via the anthropic provider) reported 70 % of
// its real tokens because the relay deflated unconditionally.
func TestForwardWithAnnotation_DeflationSkipsOpencodeClient(t *testing.T) {
	s := los39DeflationServer(t)

	opencode := los39RunAnthropicForward(t, s, map[string]string{"x-opencode-session": "ses_los39_test"})
	if got := los39InputTokens(t, opencode); got != "1000" {
		t.Errorf("opencode client saw input_tokens=%s, want 1000 (real value, no deflation)\nstream:\n%s", got, opencode)
	}

	claudeCode := los39RunAnthropicForward(t, s, map[string]string{"X-Claude-Code-Session-Id": "los39-cc-session"})
	if got := los39InputTokens(t, claudeCode); got != "700" {
		t.Errorf("Claude Code client saw input_tokens=%s, want 700 (deflated at factor 0.7)\nstream:\n%s", got, claudeCode)
	}
}

// YM-142: a request that carries neither client marker must not be deflated.
// Deflation is an opt-in for one specific client, never the default for the path.
func TestForwardWithAnnotation_DeflationSkipsUnknownClient(t *testing.T) {
	s := los39DeflationServer(t)

	unknown := los39RunAnthropicForward(t, s, nil)
	if got := los39InputTokens(t, unknown); got != "1000" {
		t.Errorf("unidentified client saw input_tokens=%s, want 1000 (no deflation without a Claude Code marker)\nstream:\n%s", got, unknown)
	}

	// Claude Code is also recognized by its user agent (older builds send no
	// session header), while an opencode user agent must never trip it.
	ua := los39RunAnthropicForward(t, s, map[string]string{"User-Agent": "claude-cli/2.1.0 (external, cli)"})
	if got := los39InputTokens(t, ua); got != "700" {
		t.Errorf("claude-cli user agent saw input_tokens=%s, want 700 (deflated)\nstream:\n%s", got, ua)
	}
	opencodeUA := los39RunAnthropicForward(t, s, map[string]string{"User-Agent": "opencode/1.2.3"})
	if got := los39InputTokens(t, opencodeUA); got != "1000" {
		t.Errorf("opencode user agent saw input_tokens=%s, want 1000 (no deflation)\nstream:\n%s", got, opencodeUA)
	}
}

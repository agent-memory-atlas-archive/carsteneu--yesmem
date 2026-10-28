package proxy

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// YM-380 (PRX-P48): three journal lines were not usable as health signals.
// proxy_health only counts what the proxy itself frames as an error, so the
// wording of these lines is the contract the counters read
// (caps/bundled-caps/proxy_health/CAP.md).

// proxyHealthCountsAsAPIError and proxyHealthCountsAsDaemonTimeout mirror the
// two rules of that cap for the plain-text cases below. The cap matches them on
// the line as written: an API error is framed by the proxy ("upstream error:",
// "upstream <4xx|5xx>:", "ping error tid="), a daemon timeout is an error with
// an "i/o timeout" that is not framed that way.
func proxyHealthCountsAsAPIError(line string) bool {
	return strings.Contains(line, "upstream error:") ||
		strings.Contains(line, "ping error tid=") ||
		strings.Contains(line, "fork ") && strings.Contains(line, ": API error:")
}

func proxyHealthCountsAsDaemonTimeout(line string) bool {
	return (strings.Contains(line, "error:") || strings.Contains(line, "err=")) &&
		strings.Contains(line, "i/o timeout") &&
		!strings.Contains(line, "upstream error:")
}

// A body that breaks off mid-read is a failed call like any other, and the
// Anthropic path logged it as a bare "read error:". With an i/o timeout that
// reads like a daemon timeout (the counter is fed by "error:...i/o timeout"
// without the upstream frame), so the failure looked like a local daemon
// problem instead of a provider failure. The OpenAI path was fixed that way in
// los 32f; this is the sibling line.
func TestLos56bAnthropicReadErrorIsFramedAsUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_1"`))
	}))
	defer upstream.Close()

	var logged bytes.Buffer
	s := &Server{
		cfg:        Config{TargetURL: upstream.URL},
		httpClient: http.DefaultClient,
		logger:     log.New(&logged, "", 0),
	}
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.forwardWithAnnotation(w, req, body, 1, nil, "yesmem", "", "", 1, false)

	line := logged.String()
	if !strings.Contains(line, "upstream error:") {
		t.Errorf("the Anthropic read error is not framed as an upstream failure, journal was:\n%s", line)
	}
	if !strings.Contains(line, "read from upstream") {
		t.Errorf("the line does not say the body broke off, journal was:\n%s", line)
	}
	if !proxyHealthCountsAsAPIError(line) {
		t.Errorf("proxy_health would not count this provider failure as an API error:\n%s", line)
	}
	if proxyHealthCountsAsDaemonTimeout(line) {
		t.Errorf("proxy_health would read this provider failure as a daemon timeout:\n%s", line)
	}
}

// forwardRaw is the bypass path and the path for bodies that do not parse. It
// relayed the upstream answer without a word in the journal: a non-200 status
// and a broken body were both invisible to the health check.
func TestLos56bForwardRawLogsTheUpstreamStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"upstream is unwell"}`))
	}))
	defer upstream.Close()

	var logged bytes.Buffer
	s := &Server{
		cfg:        Config{TargetURL: upstream.URL},
		httpClient: http.DefaultClient,
		logger:     log.New(&logged, "", 0),
	}
	body := []byte(`{"model":"claude-sonnet-4","messages":[]}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.forwardRaw(w, req, body, 1)

	line := logged.String()
	if !strings.Contains(line, "[req 1] upstream 500:") {
		t.Errorf("forwardRaw logged no upstream status line in the journal shape, journal was:\n%s", line)
	}
	if !strings.Contains(line, "upstream is unwell") {
		t.Errorf("the status line carries no body text, journal was:\n%s", line)
	}
}

// A non-200 answer whose body breaks off is one failed call, so it gets one
// journal line: proxy_health counts "upstream error:" and "upstream 5xx:" as two
// separate API errors, and a 5xx wave would then read as twice its size.
func TestLos56bForwardRawLogsOneLinePerFailedCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "200")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"half a bod`))
	}))
	defer upstream.Close()

	var logged bytes.Buffer
	s := &Server{
		cfg:        Config{TargetURL: upstream.URL},
		httpClient: http.DefaultClient,
		logger:     log.New(&logged, "", 0),
	}
	body := []byte(`{"model":"claude-sonnet-4","messages":[]}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.forwardRaw(w, req, body, 1)

	apiErrors := 0
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if proxyHealthCountsAsAPIError(line) {
			apiErrors++
		}
	}
	if apiErrors != 1 {
		t.Fatalf("proxy_health would count %d API errors for one failed call, journal was:\n%s", apiErrors, logged.String())
	}
	// The status must survive the collapsed line: it is what separates an
	// overloaded provider from a broken connection in the report.
	if !strings.Contains(logged.String(), "HTTP 500") {
		t.Errorf("the single failure line lost the upstream status, journal was:\n%s", logged.String())
	}
}

func TestLos56bForwardRawLogsABrokenBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"type\":"))
	}))
	defer upstream.Close()

	var logged bytes.Buffer
	s := &Server{
		cfg:        Config{TargetURL: upstream.URL},
		httpClient: http.DefaultClient,
		logger:     log.New(&logged, "", 0),
	}
	body := []byte(`{"model":"claude-sonnet-4","messages":[]}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.forwardRaw(w, req, body, 1)

	line := logged.String()
	if !strings.Contains(line, "upstream error:") {
		t.Errorf("forwardRaw logged no upstream failure for a broken body, journal was:\n%s", line)
	}
	if !strings.Contains(line, "read from upstream") {
		t.Errorf("the line does not say the body broke off, journal was:\n%s", line)
	}
}

// The ping error line is a journal line the cap reads. The provider body went
// into the error message verbatim, so a body with line breaks forged a second
// journal line (and a body quoting counter vocabulary moved the numbers). The
// body is foreign text and belongs on one line.
func TestLos56bKeepalivePingErrorBodyStaysOnOneLine(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("{\"error\":\"bad request\"}\n[keepalive] ping tid=forged cache_read=1 cache_write=99999\nupstream 529: overloaded"))
	}))
	defer upstream.Close()

	var logged bytes.Buffer
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target:          upstream.URL,
		Mode:            "5m",
		TimeoutOverride: 5 * time.Second,
		Logger:          log.New(&logged, "", 0),
	})

	_, _, _, _, err := ka.doHTTPPing([]byte(`{"model":"claude-sonnet-4","messages":[]}`), "key", "")
	if err == nil {
		t.Fatal("doHTTPPing returned no error for an HTTP 400 answer")
	}

	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("the ping error carries a line break, so the journal line can be forged:\n%q", msg)
	}
	if !strings.Contains(msg, "HTTP 400") {
		t.Errorf("the ping error lost its status: %q", msg)
	}
	if !strings.Contains(msg, "bad request") {
		t.Errorf("the ping error lost the body text: %q", msg)
	}
}

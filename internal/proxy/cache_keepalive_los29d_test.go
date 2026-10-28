package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// los29dLargeBody builds a Claude request whose prompt is roughly size bytes.
// The JSON wrapper is small next to the message content, so the estimate in
// the assertions stays exact enough to compare against len(buildPingBody()).
func los29dLargeBody(size int) []byte {
	return []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("x", size)))
}

// A ping is a cache read of the whole prompt. The fixed 20s client timeout that
// failed every ping above roughly 750k tokens (live 2026-10-18, YM-347) has to
// become a deadline derived from the context size. A deadline is not transmitted
// over the wire, so the assertion sits at the last point that still sees it: the
// transport the ping actually goes through.
func TestLos29dKeepaliveLargeContextGetsScaledPingTimeout(t *testing.T) {
	deadlines := make(chan time.Duration, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"type":"message","usage":{"cache_read_input_tokens":700000}}`)
	}))
	defer srv.Close()

	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target: srv.URL, Mode: "5m", Pings5m: 1, IntervalOverride: 5 * time.Millisecond,
	})
	ka.client.Transport = &los29dDeadlineTransport{base: ka.client.Transport, seen: deadlines}
	defer ka.Stop()

	ka.Reset("opencode:ses_los29d_big", los29dLargeBody(3<<20), "test-key", "")

	select {
	case rem := <-deadlines:
		if rem < 0 {
			t.Fatal("the ping carried no deadline: a context of 3 MiB is bounded only by the fixed client timeout")
		}
		if rem <= 20*time.Second {
			t.Fatalf("ping deadline = %s, want well above the old fixed 20s for a 3 MiB body", rem)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no ping reached the upstream")
	}
}

// los29dDeadlineTransport reports the remaining budget of each request and then
// sends it on through the real transport.
type los29dDeadlineTransport struct {
	base http.RoundTripper
	seen chan time.Duration
}

func (t *los29dDeadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	seen := time.Duration(-1)
	if dl, ok := req.Context().Deadline(); ok {
		seen = time.Until(dl)
	}
	select {
	case t.seen <- seen:
	default:
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// The error line has to name the context size: without it an operator cannot
// tell a 750k-token session from an upstream outage. A ping that runs into its
// own budget is reported on its own line so proxy_health can count it apart
// from real upstream failures.
func TestLos29dKeepaliveTimeoutNamesContextSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slower than the tiny budget the test passes in.
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintf(w, `{"type":"message","usage":{"cache_read_input_tokens":1}}`)
	}))
	defer srv.Close()

	logBuf := &lockedLogBuffer{}
	body := los29dLargeBody(400 << 10)
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target: srv.URL, Mode: "5m", Pings5m: 1, IntervalOverride: 5 * time.Millisecond,
		TimeoutOverride: 20 * time.Millisecond,
		Logger:          log.New(logBuf, "", 0),
	})
	defer ka.Stop()

	ka.Reset("opencode:ses_los29d_slow", body, "test-key", "")
	logBuf.waitFor(t, "ping timeout", 5*time.Second)
	ka.Stop()

	out := logBuf.String()
	if !strings.Contains(out, "ping timeout tid=opencode:ses_los29d_slow") {
		t.Fatalf("log = %q, want a 'ping timeout' line naming the thread", out)
	}
	if strings.Contains(out, "ping error tid=") {
		t.Errorf("a deadline is not an upstream error, log = %q", out)
	}
	wantTokens := len(buildPingBody(body)) / keepaliveBytesPerToken
	if !strings.Contains(out, fmt.Sprintf("ctx_tokens=%d", wantTokens)) {
		t.Errorf("log = %q, want ctx_tokens=%d", out, wantTokens)
	}
	if !regexp.MustCompile(`timeout=\S+`).MatchString(out) {
		t.Errorf("log = %q, want the budget that was exceeded", out)
	}
	if !strings.Contains(out, "context deadline exceeded") {
		t.Errorf("log = %q, want the underlying error kept", out)
	}
}

// The budget grows with the body but never without a bound: a hung upstream
// must not pin the ping slot for the lifetime of the proxy.
func TestLos29dKeepalivePingTimeoutScalesWithBodyAndStaysBounded(t *testing.T) {
	small := keepalivePingTimeout(1024)
	if small != keepalivePingTimeoutFloor {
		t.Errorf("timeout for a small body = %s, want the floor %s", small, keepalivePingTimeoutFloor)
	}
	// 3 MiB / 4 bytes per token / 10k tokens per second = 78 whole seconds.
	large := keepalivePingTimeout(3 << 20)
	if want := keepalivePingTimeoutFloor + 78*time.Second; large != want {
		t.Errorf("timeout for a 3 MiB body = %s, want %s", large, want)
	}
	if huge := keepalivePingTimeout(1 << 40); huge != keepalivePingTimeoutCeil {
		t.Errorf("timeout for a huge body = %s, want the ceiling %s", huge, keepalivePingTimeoutCeil)
	}
	// The bound also holds for the client itself: it is the hard upper limit the
	// per-request deadline is clamped to.
	if got := NewCacheKeepalive(CacheKeepaliveConfig{}).client.Timeout; got != keepalivePingTimeoutCeil {
		t.Errorf("client timeout = %s, want the ceiling %s", got, keepalivePingTimeoutCeil)
	}
	if interval := (270 * time.Second).Seconds(); keepalivePingTimeoutCeil.Seconds() >= interval {
		t.Errorf("ceiling %s must stay below the shortest ping interval (270s)", keepalivePingTimeoutCeil)
	}
}

// A dial or TLS timeout is an upstream failure, not a spent budget. The
// transport's own timeout errors satisfy errors.Is(err, context.DeadlineExceeded)
// as well (net's errTimeout matches that sentinel on purpose), so the request's
// context is the only witness: it expired for our budget and for nothing else.
func TestLos29dKeepaliveClassifiesOnlyTheOwnBudgetAsTimeout(t *testing.T) {
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()

	// A dial timeout under a live budget: upstream failure.
	if isPingTimeout(pingErrorFor(context.Background(), &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded})) {
		t.Error("a dial timeout under a live budget is not a spent context budget")
	}
	// The same error after our own deadline fired: a spent budget.
	if !isPingTimeout(pingErrorFor(expired, &net.DNSError{Err: "i/o timeout", IsTimeout: true})) {
		t.Error("a failure after the request deadline belongs to the budget")
	}
	if isPingTimeout(errors.New("HTTP 503 body={}")) {
		t.Error("an upstream answer is not a timeout")
	}
}

// A non-200 answer keeps its status even when its body breaks off mid-read: the
// status is the diagnosis, and proxy_health classifies by it.
func TestLos29dKeepaliveBrokenErrorBodyKeepsItsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "400")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error",`)
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	logBuf := &lockedLogBuffer{}
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target: srv.URL, Mode: "5m", Pings5m: 1, IntervalOverride: 5 * time.Millisecond,
		TimeoutOverride: 30 * time.Millisecond,
		Logger:          log.New(logBuf, "", 0),
	})
	defer ka.Stop()

	ka.Reset("opencode:ses_los29d_429", los29dLargeBody(64<<10), "test-key", "")
	logBuf.waitFor(t, "ping ", 5*time.Second)
	ka.Stop()

	out := logBuf.String()
	if !strings.Contains(out, "HTTP 429") {
		t.Errorf("log = %q, want the status of the error answer kept", out)
	}
	if strings.Contains(out, "ping timeout") {
		t.Errorf("log = %q, want an upstream error: the upstream answered before the budget ran out", out)
	}
}

// The budget usually runs out while the response body is read, not at the
// header: the upstream answers the headers in milliseconds, and the expensive
// part is the body. The discarded read error turned exactly that case into
// "unparsable response", which proxy_health counted as an upstream failure
// (cold review los-29d).
func TestLos29dKeepaliveBodyReadTimeoutStaysATimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "400")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"type":"message","usage":`)
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	logBuf := &lockedLogBuffer{}
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target: srv.URL, Mode: "5m", Pings5m: 1, IntervalOverride: 5 * time.Millisecond,
		TimeoutOverride: 30 * time.Millisecond,
		Logger:          log.New(logBuf, "", 0),
	})
	defer ka.Stop()

	ka.Reset("opencode:ses_los29d_body", los29dLargeBody(400<<10), "test-key", "")
	logBuf.waitFor(t, "ping ", 5*time.Second)
	ka.Stop()

	out := logBuf.String()
	if !strings.Contains(out, "ping timeout tid=opencode:ses_los29d_body") {
		t.Fatalf("log = %q, want the timeout line: the deadline ran out while the body was read", out)
	}
	if strings.Contains(out, "unparsable response") {
		t.Errorf("log = %q, want the read error kept instead of 'unparsable response'", out)
	}
}

func TestLos29dKeepaliveUpstreamFailureStaysAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error"}}`)
	}))
	defer srv.Close()

	logBuf := &lockedLogBuffer{}
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target: srv.URL, Mode: "5m", Pings5m: 1, IntervalOverride: 5 * time.Millisecond,
		Logger: log.New(logBuf, "", 0),
	})
	defer ka.Stop()

	ka.Reset("opencode:ses_los29d_503", los29dLargeBody(64<<10), "test-key", "")
	logBuf.waitFor(t, "ping error", 5*time.Second)
	ka.Stop()

	out := logBuf.String()
	if !strings.Contains(out, "ping error tid=opencode:ses_los29d_503") {
		t.Fatalf("log = %q, want the upstream failure on the ping error line", out)
	}
	if strings.Contains(out, "ping timeout") {
		t.Errorf("an HTTP 503 is not a timeout, log = %q", out)
	}
}

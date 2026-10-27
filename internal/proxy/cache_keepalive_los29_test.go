package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// los29LogBuffer is a goroutine-safe log sink: the ping runs inside a timer
// goroutine, so a plain bytes.Buffer would race with the reader.
type los29LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *los29LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *los29LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Regression YM-133: a ping that did not succeed must not be reported as a warm
// cache. The live proxy log carried 33 of 124 pings with cache_read=0,
// cache_write=0, out=0 and zero "ping error" lines, so the failure path stayed
// invisible and OnPing still refreshed the sawtooth pause timestamp.
func TestCacheKeepalive_FailedPingIsNotReportedAsWarm(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "http error status",
			status: http.StatusTooManyRequests,
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
		},
		{
			name:   "ok response without cache accounting",
			status: http.StatusOK,
			body:   `{"type":"message","content":[]}`,
		},
		{
			name:   "ok response with unparsable body",
			status: http.StatusOK,
			body:   `not json at all`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			}))
			defer srv.Close()

			var onPingCalls atomic.Int32
			logBuf := &los29LogBuffer{}
			ka := NewCacheKeepalive(CacheKeepaliveConfig{
				Target:           srv.URL,
				Mode:             "5m",
				Pings5m:          1,
				IntervalOverride: 10 * time.Millisecond,
				Logger:           log.New(logBuf, "", 0),
				OnPing: func(threadID string, cacheRead, cacheWrite, ephemeral1h int) {
					onPingCalls.Add(1)
				},
			})
			body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"test"}]}`)
			ka.Reset("opencode:ses_los29", body, "test-key", "")
			time.Sleep(200 * time.Millisecond)
			ka.Stop()

			if got := onPingCalls.Load(); got != 0 {
				t.Errorf("failed ping must not report warmth via OnPing, got %d call(s)", got)
			}
			if logged := logBuf.String(); !strings.Contains(logged, "ping error") {
				t.Errorf("failed ping must be logged as 'ping error', log=%q", logged)
			}
		})
	}
}

// The success path must keep working unchanged: a ping with cache accounting
// still feeds OnPing and keeps the normal log line.
func TestCacheKeepalive_SuccessfulPingStillReportsWarm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"type":"message","usage":{"cache_read_input_tokens":100,"cache_creation_input_tokens":0,"output_tokens":5}}`)
	}))
	defer srv.Close()

	var onPingCalls atomic.Int32
	logBuf := &los29LogBuffer{}
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target:           srv.URL,
		Mode:             "5m",
		Pings5m:          1,
		IntervalOverride: 10 * time.Millisecond,
		Logger:           log.New(logBuf, "", 0),
		OnPing: func(threadID string, cacheRead, cacheWrite, ephemeral1h int) {
			onPingCalls.Add(1)
		},
	})
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"test"}]}`)
	ka.Reset("opencode:ses_los29", body, "test-key", "")
	time.Sleep(200 * time.Millisecond)
	ka.Stop()

	if got := onPingCalls.Load(); got != 1 {
		t.Errorf("successful ping must report warmth exactly once, got %d", got)
	}
	if logged := logBuf.String(); strings.Contains(logged, "ping error") {
		t.Errorf("successful ping must not log an error, log=%q", logged)
	}
}

// Cold review 5.2 finding 1: the ping path passed ephemeral_1h as a hardcoded 0,
// so a ping that rewrote the prefix (cache expired) looked like "the API does not
// honor 1h" and demoted a confirmed detection, which then dragged the pause
// threshold back to 4 minutes. The ping response carries its own 1h figure.
func TestCacheKeepalive_OnPingCarriesEphemeral1h(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"type":"message","usage":{"cache_read_input_tokens":0,"cache_creation_input_tokens":50000,"cache_creation":{"ephemeral_1h_input_tokens":50000}}}`)
	}))
	defer srv.Close()

	got := make(chan int, 1)
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target:           srv.URL,
		Mode:             "5m",
		Pings5m:          1,
		IntervalOverride: 10 * time.Millisecond,
		OnPing: func(threadID string, cacheRead, cacheWrite, ephemeral1h int) {
			select {
			case got <- ephemeral1h:
			default:
			}
		},
	})
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"test"}]}`)
	ka.Reset("opencode:ses_los29", body, "test-key", "")
	time.Sleep(200 * time.Millisecond)
	ka.Stop()

	select {
	case ephemeral1h := <-got:
		if ephemeral1h != 50000 {
			t.Errorf("OnPing must carry the ping's ephemeral_1h, got %d", ephemeral1h)
		}
	default:
		t.Fatal("no ping was reported")
	}
}

// A confirmed 1h detection must survive a cold ping whose prefix write is
// 1h-cached: the ping is evidence FOR the 1h cache, not against it.
func TestServer_OnKeepalivePingKeepsConfirmedTTL(t *testing.T) {
	s := &Server{cfg: Config{CacheTTL: "ephemeral", TokenThreshold: 100000, TokenMinimumThreshold: 1000}}
	s.cacheTTLDetector = NewCacheTTLDetector()
	s.sawtoothTrigger = NewSawtoothTrigger(cacheGapForTTL("ephemeral"), 100000, 1000)
	s.frozenStubs = NewFrozenStubsWithTTL(sawtoothTTLForCacheTTL("ephemeral"))
	s.logger = log.New(io.Discard, "", 0)

	confirmed := true
	s.cacheTTLDetector.supported = &confirmed
	s.applyDetectedCacheTTL(true)

	s.onKeepalivePing("opencode:ses_los29A", 0, 50000, 50000)

	sup := s.cacheTTLDetector.Is1hSupported()
	if sup == nil || !*sup {
		t.Fatal("a 1h-cached prefix write must not demote a confirmed 1h detection")
	}
	if got := s.sawtoothTrigger.pauseThreshold; got != 61*time.Minute {
		t.Errorf("pause threshold must stay at the 1h gap, got %v", got)
	}
}

// Consequence review 5.3 finding 1: Claude Code on a subscription authenticates
// with "Authorization: Bearer <oauth token>". The ping sent that raw value in
// x-api-key, so every ping for a now-enabled Claude Code session failed.
func TestCacheKeepalive_PingAuthHeaderPerCredentialForm(t *testing.T) {
	cases := []struct {
		name        string
		credential  string
		wantAuth    string
		wantXAPIKey string
	}{
		{"oauth bearer", "Bearer sk-ant-oat01-abc", "Bearer sk-ant-oat01-abc", ""},
		{"api key", "sk-ant-" + "api03-abc", "", "sk-ant-" + "api03-abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := make(chan [2]string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case got <- [2]string{r.Header.Get("Authorization"), r.Header.Get("x-api-key")}:
				default:
				}
				fmt.Fprint(w, `{"type":"message","usage":{"cache_read_input_tokens":100,"cache_creation_input_tokens":0,"output_tokens":5}}`)
			}))
			defer srv.Close()

			ka := NewCacheKeepalive(CacheKeepaliveConfig{
				Target: srv.URL, Mode: "5m", Pings5m: 1, IntervalOverride: 10 * time.Millisecond,
			})
			body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"test"}]}`)
			ka.Reset("opencode:ses_los29", body, c.credential, "")
			time.Sleep(200 * time.Millisecond)
			ka.Stop()

			select {
			case h := <-got:
				if h[0] != c.wantAuth || h[1] != c.wantXAPIKey {
					t.Errorf("auth headers = (Authorization %q, x-api-key %q), want (%q, %q)", h[0], h[1], c.wantAuth, c.wantXAPIKey)
				}
			default:
				t.Fatal("no ping reached the server")
			}
		})
	}
}

// Consequence review 5.3 finding 4: the ping that discovers the 1h cache is the
// last ping of the phase, so a spent thread must still be re-armed by the
// detection change — keepalive exists for exactly that idle thread.
func TestCacheKeepalive_DetectionChangeRearmsSpentThread(t *testing.T) {
	var pingCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pingCount.Add(1)
		fmt.Fprint(w, `{"type":"message","usage":{"cache_read_input_tokens":100,"cache_creation_input_tokens":0,"output_tokens":5}}`)
	}))
	defer srv.Close()

	det := NewCacheTTLDetector()
	f := false
	det.supported = &f
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target:           srv.URL,
		Mode:             "auto",
		Pings5m:          1,
		Pings1h:          1,
		Detector:         det,
		IntervalOverride: 10 * time.Millisecond,
	})
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"test"}]}`)
	ka.Reset("opencode:ses_los29", body, "test-key", "")
	time.Sleep(120 * time.Millisecond)
	if got := pingCount.Load(); got != 1 {
		t.Fatalf("expected exactly one ping in the phase, got %d", got)
	}

	// Detection flips: that is a new keepalive phase, not a free re-ping.
	tr := true
	det.supported = &tr
	ka.Retrigger()
	time.Sleep(120 * time.Millisecond)
	ka.Stop()

	if got := pingCount.Load(); got != 2 {
		t.Errorf("a detection change must start a new phase, got %d pings", got)
	}
}

// Consequence review 5.3 finding 7: two goroutines must not both apply the same
// transition (double propagation, double retrigger).
func TestCacheTTLDetector_ChangeReportedExactlyOnce(t *testing.T) {
	det := NewCacheTTLDetector()
	det.mu.Lock()
	det.threadRequests["opencode:ses_los29A"] = time.Now().Add(-7 * time.Minute)
	det.mu.Unlock()

	if _, changed := det.RecordResponseAndReportChange("opencode:ses_los29A", 100000, 2000, 2000); !changed {
		t.Fatal("the first gap response must report a change")
	}
	if _, changed := det.RecordResponseAndReportChange("opencode:ses_los29A", 100000, 2000, 2000); changed {
		t.Error("the same result must not be reported as a change twice")
	}
}

// Security review 5.4 finding 1: a detection change refilled the ping budget of
// every thread, so an upstream flipping its evidence could keep the phase alive
// forever (each ping resends the prompt and the credential). The refill is
// rate-limited, the TTL propagation is not.
func TestServer_DetectionChangeRefillIsRateLimited(t *testing.T) {
	var pingCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pingCount.Add(1)
		fmt.Fprint(w, `{"type":"message","usage":{"cache_read_input_tokens":100,"cache_creation_input_tokens":0,"output_tokens":5}}`)
	}))
	defer srv.Close()

	s := &Server{cfg: Config{CacheTTL: "ephemeral", TokenThreshold: 100000, TokenMinimumThreshold: 1000}}
	s.cacheTTLDetector = NewCacheTTLDetector()
	s.sawtoothTrigger = NewSawtoothTrigger(cacheGapForTTL("ephemeral"), 100000, 1000)
	s.frozenStubs = NewFrozenStubsWithTTL(sawtoothTTLForCacheTTL("ephemeral"))
	s.logger = log.New(io.Discard, "", 0)
	s.cacheKeepalive = NewCacheKeepalive(CacheKeepaliveConfig{
		Target: srv.URL, Mode: "5m", Pings5m: 1, IntervalOverride: 10 * time.Millisecond,
	})
	defer s.cacheKeepalive.Stop()

	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"test"}]}`)
	s.cacheKeepalive.Reset("opencode:ses_los29A", body, "test-key", "")
	time.Sleep(100 * time.Millisecond)
	if got := pingCount.Load(); got != 1 {
		t.Fatalf("expected the phase to spend its one ping, got %d", got)
	}

	// A refill happened just now: the flip still moves the TTL, but it must not
	// hand out another ping phase.
	s.lastKeepaliveRetrigger = time.Now()
	s.cacheTTLDetector.mu.Lock()
	s.cacheTTLDetector.threadRequests["opencode:ses_los29A"] = time.Now().Add(-7 * time.Minute)
	s.cacheTTLDetector.mu.Unlock()
	s.recordCacheResponse("opencode:ses_los29A", 100000, 2000, 2000)

	sup := s.cacheTTLDetector.Is1hSupported()
	if sup == nil || !*sup {
		t.Fatal("the flip itself must still be recorded")
	}
	if got := s.sawtoothTrigger.pauseThreshold; got != 61*time.Minute {
		t.Errorf("the TTL propagation must not be rate-limited, pause threshold = %v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := pingCount.Load(); got != 1 {
		t.Errorf("a flip inside the cooldown must not open a new phase, got %d pings", got)
	}

	// Outside the cooldown the same mechanism must still re-arm the thread.
	s.lastKeepaliveRetrigger = time.Now().Add(-2 * keepaliveRetriggerCooldown)
	s.recordCacheResponse("opencode:ses_los29A", 0, 50000, 0) // 5m evidence flips back
	time.Sleep(100 * time.Millisecond)

	if got := pingCount.Load(); got != 2 {
		t.Errorf("a flip outside the cooldown must start a new phase, got %d pings", got)
	}
}

package proxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression YM-272, part 1: the cache gate was built once from cfg.CacheTTL and
// never followed the detected TTL. A confirmed 1h cache therefore still disabled
// caching after 4 minutes, exactly like an unconfirmed 5 minute cache.
func TestServer_DetectedTTL_MovesCacheGateGap(t *testing.T) {
	s := &Server{cfg: Config{CacheTTL: "ephemeral", TokenThreshold: 100000, TokenMinimumThreshold: 1000}}
	s.cacheTTLDetector = NewCacheTTLDetector()
	s.cacheGate = NewCacheGate(cacheGapForTTL("ephemeral"))
	s.sawtoothTrigger = NewSawtoothTrigger(cacheGapForTTL("ephemeral"), 100000, 1000)
	s.frozenStubs = NewFrozenStubsWithTTL(sawtoothTTLForCacheTTL("ephemeral"))
	s.logger = log.New(io.Discard, "", 0)

	// A 7 minute gap with a warm cache proves the 1h lifetime.
	s.cacheTTLDetector.mu.Lock()
	s.cacheTTLDetector.threadRequests["opencode:ses_los29bGate"] = time.Now().Add(-7 * time.Minute)
	s.cacheTTLDetector.mu.Unlock()

	s.recordCacheResponse("opencode:ses_los29bGate", 100000, 2000, 0)

	if sup := s.cacheTTLDetector.Is1hSupported(); sup == nil || !*sup {
		t.Fatalf("test premise broken: detection did not flip to 1h, got %v", sup)
	}
	// 30 minutes sits between the 5 minute and the 1 hour lifetime: with the
	// detected 1h cache the next request still warms the same prefix.
	s.cacheGate.lastRequest = time.Now().Add(-30 * time.Minute)
	if !s.cacheGate.ShouldCache() {
		t.Fatal("cache gate still disables caching 30 minutes after the last request, detection said 1h")
	}
}

// Regression YM-272, part 1 (b): the OpenAI parity runtime state builds the same
// derivatives lazily for a hand-assembled context and took cfg.CacheTTL, ignoring
// an already known detection.
func TestServer_LazyOpenAIStateUsesDetectedTTL(t *testing.T) {
	s := &Server{cfg: Config{CacheTTL: "ephemeral", TokenThreshold: 100000, TokenMinimumThreshold: 1000}}
	s.cacheTTLDetector = NewCacheTTLDetector()
	confirmed := true
	s.cacheTTLDetector.mu.Lock()
	s.cacheTTLDetector.supported = &confirmed
	s.cacheTTLDetector.mu.Unlock()

	s.ensureOpenAIRuntimeState()

	if s.cacheGate == nil {
		t.Fatal("cache gate not created")
	}
	// 30 minutes is outside the 5 minute lifetime and inside the 1 hour one.
	s.cacheGate.lastRequest = time.Now().Add(-30 * time.Minute)
	if !s.cacheGate.ShouldCache() {
		t.Fatal("lazily created cache gate runs on cfg.CacheTTL instead of the detected 1h lifetime")
	}
}

// Regression YM-272, part 4 (b): the ping client followed redirects and Go
// copies x-api-key to the redirect target, so an upstream redirect handed the
// credential to a host the operator never configured.
func TestCacheKeepalive_PingDoesNotFollowRedirect(t *testing.T) {
	var mu sync.Mutex
	leaked := ""
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = r.Header.Get("x-api-key")
		mu.Unlock()
		fmt.Fprint(w, `{"usage":{"cache_read_input_tokens":10,"cache_creation_input_tokens":5,"output_tokens":1}}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/messages", http.StatusFound)
	}))
	defer redirector.Close()

	ka := NewCacheKeepalive(CacheKeepaliveConfig{Target: redirector.URL})
	_, _, _, _, err := ka.doHTTPPing([]byte(`{"model":"claude-sonnet-4-5"}`), "sk-ant-secret-limit", "")

	mu.Lock()
	got := leaked
	mu.Unlock()
	if got != "" {
		t.Fatalf("the ping credential reached the redirect target: %q", got)
	}
	if err == nil {
		t.Fatal("a redirect was reported as a successful ping")
	}
}

// Regression YM-272, part 4 (a): the ping response body was read without a
// limit. The body is upstream-controlled and a ping asks for up to 8000 output
// tokens, so an unbounded read is an unbounded allocation on every ping.
func TestCacheKeepalive_PingResponseBodyIsBounded(t *testing.T) {
	body := &los29bCountingBody{remaining: 4 << 20}
	ka := NewCacheKeepalive(CacheKeepaliveConfig{Target: "http://upstream.invalid"})
	ka.client = &http.Client{Transport: los29bRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
			Request:    req,
		}, nil
	})}

	_, _, _, _, err := ka.doHTTPPing([]byte(`{"model":"claude-sonnet-4-5"}`), "sk-ant", "")

	// One byte past the limit is read to tell a truncated body from an exact fit.
	if body.read > keepaliveMaxResponseBytes+1 {
		t.Fatalf("ping read %d bytes of the response, want at most %d", body.read, keepaliveMaxResponseBytes+1)
	}
	if err == nil {
		t.Fatal("a truncated response body was accepted as a successful ping")
	}
}

// Regression YM-272, part 4 (c): Retrigger re-armed every known thread,
// including threads whose last request was long past threadEvictionAge. That
// resurrected evicted sessions and their full prompt bodies.
func TestCacheKeepalive_RetriggerDropsExpiredThreads(t *testing.T) {
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target:           "http://upstream.invalid",
		Mode:             "5m",
		Pings5m:          1,
		IntervalOverride: time.Hour,
	})
	defer ka.Stop()

	ka.threads["opencode:ses_los29bExpired"] = &threadState{
		body:     []byte(`{"model":"claude-sonnet-4-5"}`),
		lastUsed: time.Now().Add(-3 * time.Hour),
	}
	ka.threads["opencode:ses_los29bActive"] = &threadState{
		body:     []byte(`{"model":"claude-sonnet-4-5"}`),
		lastUsed: time.Now(),
	}

	ka.Retrigger()

	if _, ok := ka.threads["opencode:ses_los29bExpired"]; ok {
		t.Fatal("retrigger re-armed a thread past the eviction age")
	}
	if _, ok := ka.threads["opencode:ses_los29bActive"]; !ok {
		t.Fatal("retrigger dropped a live thread")
	}
}

// Regression YM-272, part 2 (a): threadRequests kept one entry per thread
// forever. The thread ID comes from a client header, so the map grew without
// bound; an expired timeline is useless because the gap evidence only lives
// between 5 minutes and the 1h lifetime.
func TestCacheTTLDetector_EvictsExpiredThreadTimelines(t *testing.T) {
	det := NewCacheTTLDetector()
	det.threadRequests["opencode:ses_los29bStale"] = time.Now().Add(-3 * time.Hour)
	det.threadRequests["opencode:ses_los29bRecent"] = time.Now().Add(-10 * time.Minute)

	det.RecordResponse("opencode:ses_los29bNew", 1000, 1000, 1000)

	det.mu.Lock()
	defer det.mu.Unlock()
	if _, ok := det.threadRequests["opencode:ses_los29bStale"]; ok {
		t.Fatal("an expired timeline was never evicted")
	}
	if _, ok := det.threadRequests["opencode:ses_los29bRecent"]; !ok {
		t.Fatal("a timeline inside the eviction age was dropped, it is the gap evidence")
	}
	if _, ok := det.threadRequests["opencode:ses_los29bNew"]; !ok {
		t.Fatal("the timeline of the current response was dropped")
	}
}

// Regression YM-272, part 2 (b): the age rule alone cannot bound the map when a
// caller invents a fresh thread ID per request, so the map needs a hard cap.
func TestCacheTTLDetector_CapsThreadTimelines(t *testing.T) {
	det := NewCacheTTLDetector()
	now := time.Now()
	// Staggered inside the eviction age: only the hard cap can bound this map,
	// and the oldest entry is the one that must go first.
	newest, oldest := "invented:ses_los29b0", ""
	for i := 0; i < 5000; i++ {
		tid := fmt.Sprintf("invented:ses_los29b%d", i)
		det.threadRequests[tid] = now.Add(-time.Duration(i) * time.Second)
		oldest = tid
	}

	det.RecordResponse("opencode:ses_los29bReal", 1000, 1000, 1000)

	det.mu.Lock()
	defer det.mu.Unlock()
	if len(det.threadRequests) > ttlMaxTrackedThreads {
		t.Fatalf("thread timeline map holds %d entries after a response, want at most %d", len(det.threadRequests), ttlMaxTrackedThreads)
	}
	if _, ok := det.threadRequests["opencode:ses_los29bReal"]; !ok {
		t.Fatal("the cap dropped the timeline of the response that triggered it")
	}
	if _, ok := det.threadRequests[oldest]; ok {
		t.Fatal("the oldest timeline survived the cap, the eviction order is arbitrary")
	}
	if _, ok := det.threadRequests[newest]; !ok {
		t.Fatal("the newest timeline was dropped")
	}
}

// Regression aus der Kaltreview zu YM-272: ka.threads hatte nur eine Altersgrenze
// und pro Eintrag einen vollstaendigen Prompt-Body. Ein Client, der innerhalb von
// zwei Stunden Thread-IDs erfindet, waechst damit unbegrenzt, dieselbe Klasse wie
// der ungebremste Zeitstempel-Map in cache_ttl_detect.go.
func TestCacheKeepalive_BoundsThreadStates(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5"}`)
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target:           "http://upstream.invalid",
		Mode:             "5m",
		Pings5m:          1,
		IntervalOverride: time.Hour,
	})
	defer ka.Stop()

	for i := 0; i < 600; i++ {
		ka.Reset(fmt.Sprintf("opencode:ses_los29b%032d", i), body, "sk-ant", "")
	}

	ka.mu.Lock()
	n := len(ka.threads)
	ka.mu.Unlock()
	if n > keepaliveMaxThreads {
		t.Fatalf("keepalive holds %d thread states with their prompt bodies, want at most %d", n, keepaliveMaxThreads)
	}
}

// The cap must drop the oldest phases first: a thread that stopped asking long
// ago is the one whose stored prompt body nobody needs any more.
func TestCacheKeepalive_CapDropsOldestThreadStates(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5"}`)
	ka := NewCacheKeepalive(CacheKeepaliveConfig{
		Target:           "http://upstream.invalid",
		Mode:             "5m",
		Pings5m:          1,
		IntervalOverride: time.Hour,
	})
	defer ka.Stop()

	now := time.Now()
	newest, oldest := "opencode:ses_los29bNewest", ""
	ka.mu.Lock()
	for i := 0; i < 600; i++ {
		tid := fmt.Sprintf("opencode:ses_los29bSeeded%d", i)
		ka.threads[tid] = &threadState{body: body, lastUsed: now.Add(-time.Duration(i) * time.Second)}
		oldest = tid
	}
	ka.mu.Unlock()

	ka.Reset(newest, body, "sk-ant", "")

	ka.mu.Lock()
	defer ka.mu.Unlock()
	if _, ok := ka.threads[oldest]; ok {
		t.Fatal("the oldest thread state survived the cap, the eviction order is arbitrary")
	}
	if _, ok := ka.threads[newest]; !ok {
		t.Fatal("the thread of the triggering reset was dropped")
	}
}

// Regression aus der Security-Review zu YM-272: die Groessenpruefung lief vor
// der Statuspruefung, deshalb verschwand bei einer zu grossen Fehlerantwort der
// HTTP-Status aus der Diagnose.
func TestCacheKeepalive_OversizedErrorKeepsItsStatus(t *testing.T) {
	body := &los29bCountingBody{remaining: 2 << 20}
	ka := NewCacheKeepalive(CacheKeepaliveConfig{Target: "http://upstream.invalid"})
	ka.client = &http.Client{Transport: los29bRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusRequestEntityTooLarge,
			Header:     make(http.Header),
			Body:       body,
			Request:    req,
		}, nil
	})}

	_, _, _, _, err := ka.doHTTPPing([]byte(`{"model":"claude-sonnet-4-5"}`), "sk-ant", "")

	if err == nil {
		t.Fatal("an oversized error response was accepted as a successful ping")
	}
	if !strings.Contains(err.Error(), "413") {
		t.Fatalf("error text lost the HTTP status: %v", err)
	}
}

// los29bCountingBody serves a fixed number of filler bytes and records how many
// of them the caller consumed.
type los29bCountingBody struct {
	remaining int
	read      int
}

func (b *los29bCountingBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > b.remaining {
		n = b.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = ' '
	}
	b.remaining -= n
	b.read += n
	return n, nil
}

func (b *los29bCountingBody) Close() error { return nil }

type los29bRoundTripper func(*http.Request) (*http.Response, error)

func (f los29bRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

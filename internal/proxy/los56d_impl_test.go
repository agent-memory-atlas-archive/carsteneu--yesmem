package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// YM-322 (3): increment_fail ran on every request of an error loop, so a single
// repeated build failure pushed a used learning to fail_count 5 within five
// turns and the PreToolUse hook started hard-blocking on it.
func TestLos56dFailCounterCountsOneErrorOnce(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0), failFingerprints: make(map[string]map[int64]uint64)}

	ids := []int64{7, 8}
	if got := s.freshFailIDs("tid", ids, "build failed: undefined: foo"); len(got) != 2 {
		t.Fatalf("first error: ids = %v, want both", got)
	}
	if got := s.freshFailIDs("tid", ids, "build failed: undefined: foo"); len(got) != 0 {
		t.Errorf("identical error counted twice: %v", got)
	}
	if got := s.freshFailIDs("tid", ids, "build failed: undefined: bar"); len(got) != 2 {
		t.Errorf("a new error must count again, ids = %v", got)
	}
	if got := s.freshFailIDs("other", ids, "build failed: undefined: foo"); len(got) != 2 {
		t.Errorf("another thread must count on its own, ids = %v", got)
	}
}

// YM-322 (4b): the is_error predicate existed in five places with drifting
// text handling. One helper is the single source.
func TestLos56dToolResultIsErrorPredicate(t *testing.T) {
	cases := []struct {
		name  string
		block map[string]any
		want  bool
	}{
		{"flag true", map[string]any{"type": "tool_result", "is_error": true}, true},
		{"flag false", map[string]any{"type": "tool_result", "is_error": false}, false},
		{"flag absent", map[string]any{"type": "tool_result"}, false},
		{"not a tool result", map[string]any{"type": "text", "is_error": true}, false},
		{"string flag", map[string]any{"type": "tool_result", "is_error": "true"}, false},
	}
	for _, tc := range cases {
		if got := toolResultIsError(tc.block); got != tc.want {
			t.Errorf("%s: toolResultIsError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- YM-331 ---

// YM-331 (1): timestampStore.threads grows with every thread and every message
// of that thread, and it is persisted. A proxy that serves sessions for weeks
// accumulated an entry per message per thread with no upper bound.
func TestLos56dTimestampStoreIsBounded(t *testing.T) {
	ts := NewTimestampStore()
	for i := 0; i < 900; i++ {
		thread := "tid-" + strconv.Itoa(i)
		for m := 1; m <= 5; m++ {
			ts.Store(thread, m, &TimestampMeta{Timestamp: "Mo 2026-10-20 10:00:00", Rules: "some rules text"})
		}
	}

	removed := ts.CapState(100, 3)
	if removed == 0 {
		t.Fatal("CapState removed nothing above the cap")
	}

	ts.mu.RLock()
	threads := len(ts.threads)
	_, newestKept := ts.threads["tid-899"]
	var tooMany int
	for _, tt := range ts.threads {
		if len(tt.Entries) > 3 {
			tooMany++
		}
	}
	ts.mu.RUnlock()

	if threads > 100 {
		t.Errorf("threads = %d, want <= 100", threads)
	}
	if tooMany > 0 {
		t.Errorf("%d thread(s) keep more than 3 message entries", tooMany)
	}
	if !newestKept {
		t.Error("the last written thread was evicted")
	}
}

// YM-331 (1b): the other per-thread maps of the package carry the same
// client-writable key and had no cap at all; loopStates had no delete either.
func TestLos56dThreadStateMapsAreBounded(t *testing.T) {
	s := los26aHandlerServer(t, "http://127.0.0.1:1")
	s.logger = log.New(io.Discard, "", 0)
	s.threadThresholdOverrides = make(map[string]int)

	for i := 0; i < maxThreadStates+500; i++ {
		tid := "tid-" + strconv.Itoa(i)
		s.loopStates[tid] = &LoopState{WarningCount: 1}
		s.responseTimes[tid] = time.Now()
		s.thinkCounters[tid] = 1
		s.lastInjectedIDs[tid] = map[int64]string{1: "briefing"}
		s.sessionInjectCounts[tid] = map[int64]int{1: 1}
		s.lastTurnInjected[tid] = map[int64]bool{1: true}
		s.channelInjectCount[tid] = 1
		s.threadThresholdOverrides[tid] = 12345
		s.msgCounters.counts[tid] = 7
		s.forkState.threads[tid] = &threadForkState{}
		s.cacheStatusWriter.threads[tid] = &statusThreadState{}
		s.eagerStubMemory.stubbed[tid] = map[string]bool{"t1": true}
	}

	s.pruneThreadStateMaps()

	checks := map[string]int{
		"loopStates":               len(s.loopStates),
		"responseTimes":            len(s.responseTimes),
		"thinkCounters":            len(s.thinkCounters),
		"lastInjectedIDs":          len(s.lastInjectedIDs),
		"sessionInjectCounts":      len(s.sessionInjectCounts),
		"lastTurnInjected":         len(s.lastTurnInjected),
		"channelInjectCount":       len(s.channelInjectCount),
		"threadThresholdOverrides": len(s.threadThresholdOverrides),
		"msgCounters":              len(s.msgCounters.counts),
		"forkState":                len(s.forkState.threads),
		"cacheStatus.threads":      len(s.cacheStatusWriter.threads),
		"eagerStubMemory":          len(s.eagerStubMemory.stubbed),
	}
	for name, n := range checks {
		if n > maxThreadStates {
			t.Errorf("%s = %d entries after prune, cap is %d", name, n, maxThreadStates)
		}
	}
}

// --- YM-339 ---

// YM-339 (1): the forwarding client followed redirects, so a redirect response
// from the upstream carried the Authorization header and x-api-key to whatever
// host the Location named. The keepalive client was hardened that way in Los
// 29b; the forwarding client was not.
func TestLos56dForwardClientDoesNotFollowRedirects(t *testing.T) {
	var reached int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reached, 1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			t.Errorf("redirect target received credentials: %v", r.Header)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(upstream.Close)

	req, err := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer super-secret")
	req.Header.Set("x-api-key", "also-secret")

	resp, err := newProxyHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if got := atomic.LoadInt64(&reached); got != 0 {
		t.Errorf("forward client followed the redirect (%d requests reached the target)", got)
	}
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the upstream's 307 to be handed back", resp.StatusCode)
	}
}

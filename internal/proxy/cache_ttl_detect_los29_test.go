package proxy

import (
	"io"
	"log"
	"testing"
	"time"
)

// Regression YM-134 (part 1): the response of thread A was attributed to
// whichever thread called RecordRequest last, so with two interleaved threads
// the gap measurement silently landed on the wrong thread and a confirmed 1h
// cache was missed.
func TestCacheTTLDetector_InterleavedThreadsKeepTheirOwnGap(t *testing.T) {
	det := NewCacheTTLDetector()
	det.mu.Lock()
	det.threadRequests["opencode:ses_los29A"] = time.Now().Add(-7 * time.Minute)
	det.mu.Unlock()

	// Thread B answers before A's response arrives: its timestamp must not
	// become the timeline for A's gap.
	det.RecordResponse("opencode:ses_los29B", 1000, 1000, 1000)

	// A's response: warm cache after A's own 7 minute gap.
	det.RecordResponse("opencode:ses_los29A", 100000, 2000, 0)

	sup := det.Is1hSupported()
	if sup == nil || !*sup {
		t.Fatal("A's response was measured against B's timeline: 1h not confirmed")
	}
}

// The gap must also stay unknown for a thread that never answered before, even
// when another thread's response just updated its own timestamp.
func TestCacheTTLDetector_OtherThreadGapDoesNotLeak(t *testing.T) {
	det := NewCacheTTLDetector()
	det.mu.Lock()
	det.threadRequests["opencode:ses_los29A"] = time.Now().Add(-7 * time.Minute)
	det.mu.Unlock()

	det.RecordResponse("opencode:ses_los29B", 100000, 2000, 0) // B's first response: no prior gap

	if sup := det.Is1hSupported(); sup != nil {
		t.Fatalf("first response of B must stay inconclusive, got %v", *sup)
	}
}

// Regression YM-134 (part 2): a confirmed 1h cache must move the sawtooth pause
// threshold with it. Otherwise every idle thread triggers a stub-cycle after 4
// minutes even though the cache stays warm for an hour.
func TestServer_RecordCacheResponseMovesSawtoothPauseThreshold(t *testing.T) {
	s := &Server{cfg: Config{CacheTTL: "ephemeral", TokenThreshold: 100000, TokenMinimumThreshold: 1000}}
	s.cacheTTLDetector = NewCacheTTLDetector()
	s.sawtoothTrigger = NewSawtoothTrigger(cacheGapForTTL("ephemeral"), 100000, 1000)
	s.frozenStubs = NewFrozenStubsWithTTL(sawtoothTTLForCacheTTL("ephemeral"))
	s.logger = log.New(io.Discard, "", 0)

	s.cacheTTLDetector.mu.Lock()
	s.cacheTTLDetector.threadRequests["opencode:ses_los29A"] = time.Now().Add(-7 * time.Minute)
	s.cacheTTLDetector.mu.Unlock()

	// Warm response after the thread's own 7 minute gap → 1h confirmed.
	s.recordCacheResponse("opencode:ses_los29A", 100000, 2000, 0)

	if got := s.sawtoothTrigger.pauseThreshold; got != 61*time.Minute {
		t.Fatalf("pause threshold must follow the detected 1h cache, got %v", got)
	}

	// Behaviour: a 10 minute idle thread below the token threshold is not a pause.
	s.sawtoothTrigger.UpdateAfterResponse("opencode:ses_los29A", 50000, 10)
	s.sawtoothTrigger.mu.Lock()
	s.sawtoothTrigger.lastRequestTime["opencode:ses_los29A"] = time.Now().Add(-10 * time.Minute)
	s.sawtoothTrigger.mu.Unlock()
	if got := s.sawtoothTrigger.ShouldTrigger("opencode:ses_los29A", 0, 100000); got != TriggerNone {
		t.Errorf("a 10 minute idle thread with a 1h cache must not pause-trigger, got %v", got)
	}
}

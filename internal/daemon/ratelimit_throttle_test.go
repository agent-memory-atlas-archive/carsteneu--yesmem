package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/proxy"
	"github.com/carsteneu/yesmem/internal/storage"
)

// The daemon carried its own copy of the utilization arithmetic at two sites
// (the LLM throttle function and the batch extraction loop), both blind to the
// age of the snapshot they read. The shared helper is the single place that
// decides, so a new site cannot re-introduce a third copy.
func TestProxyRateLimitThrottled(t *testing.T) {
	// Each subtest gets its own store: the shared one made the "no snapshot" case
	// depend on subtest order.
	newStore := func(t *testing.T) *storage.Store {
		t.Helper()
		s, err := storage.Open(":memory:")
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}

	persist := func(t *testing.T, s *storage.Store, rl *proxy.RateLimitInfo) {
		t.Helper()
		raw, err := json.Marshal(rl)
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		if err := s.SetProxyState("rate_limits", string(raw)); err != nil {
			t.Fatalf("persist snapshot: %v", err)
		}
	}

	t.Run("no snapshot", func(t *testing.T) {
		if proxyRateLimitThrottled(newStore(t), 0.5) {
			t.Error("without a snapshot nothing may be throttled")
		}
	})

	t.Run("fresh high utilization", func(t *testing.T) {
		s := newStore(t)
		persist(t, s, &proxy.RateLimitInfo{
			Unified5hUtilization: 0.60,
			IsSubscription:       true,
			ParsedAt:             time.Now(),
		})
		if !proxyRateLimitThrottled(s, 0.5) {
			t.Error("60% utilization must throttle at a 50% threshold")
		}
	})

	t.Run("fresh low utilization", func(t *testing.T) {
		s := newStore(t)
		persist(t, s, &proxy.RateLimitInfo{
			Unified5hUtilization: 0.20,
			IsSubscription:       true,
			ParsedAt:             time.Now(),
		})
		if proxyRateLimitThrottled(s, 0.5) {
			t.Error("20% utilization must not throttle at a 50% threshold")
		}
	})

	t.Run("stale high utilization", func(t *testing.T) {
		s := newStore(t)
		persist(t, s, &proxy.RateLimitInfo{
			Unified5hUtilization: 0.95,
			IsSubscription:       true,
			ParsedAt:             time.Now().Add(-6 * time.Hour),
		})
		if proxyRateLimitThrottled(s, 0.5) {
			t.Error("a 6h old snapshot must not throttle: its window has passed")
		}
	})

	t.Run("future snapshot", func(t *testing.T) {
		s := newStore(t)
		persist(t, s, &proxy.RateLimitInfo{
			Unified5hUtilization: 0.95,
			IsSubscription:       true,
			ParsedAt:             time.Now().Add(3 * time.Hour),
		})
		if proxyRateLimitThrottled(s, 0.5) {
			t.Error("a snapshot dated in the future must not throttle")
		}
	})

	t.Run("malformed snapshot", func(t *testing.T) {
		s := newStore(t)
		if err := s.SetProxyState("rate_limits", "{not json"); err != nil {
			t.Fatalf("persist malformed snapshot: %v", err)
		}
		if proxyRateLimitThrottled(s, 0.5) {
			t.Error("a malformed snapshot must not throttle")
		}
	})
}

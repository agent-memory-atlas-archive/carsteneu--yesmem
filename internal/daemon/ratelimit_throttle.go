package daemon

import (
	"encoding/json"

	"github.com/carsteneu/yesmem/internal/proxy"
	"github.com/carsteneu/yesmem/internal/storage"
)

// readRateLimits returns the last rate-limit snapshot the proxy persisted, or
// nil when there is none or it is unusable. The proxy writes the JSON form of
// proxy.RateLimitInfo through the _persist_rate_limits RPC.
func readRateLimits(store *storage.Store) *proxy.RateLimitInfo {
	raw, err := store.GetProxyState("rate_limits")
	if err != nil || raw == "" {
		return nil
	}
	var rl proxy.RateLimitInfo
	if json.Unmarshal([]byte(raw), &rl) != nil {
		return nil
	}
	return &rl
}

// proxyRateLimitThrottled reports whether the persisted snapshot puts API
// utilization above threshold. The rule lives in proxy.RateLimitInfo, so the
// daemon neither duplicates the arithmetic nor loses the staleness check: an
// old snapshot never throttles.
func proxyRateLimitThrottled(store *storage.Store, threshold float64) bool {
	rl := readRateLimits(store)
	return rl != nil && rl.ShouldThrottle(threshold)
}

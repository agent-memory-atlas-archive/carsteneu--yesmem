package proxy

import (
	"encoding/json"
	"sync"
)

// CapsCache caches the daemon's get_active_caps response per thread.
// When the active-capabilities list is unchanged the rendered injection
// block stays byte-identical turn-to-turn so Anthropic's prompt cache hits
// on the full header prefix. Invalidation belongs on activation changes —
// the frozenStubs.Invalidate(threadID) call site is the natural hook
// (wired in Cycle D).
type CapsCache struct {
	mu      sync.RWMutex
	entries map[string][]byte
}

func NewCapsCache() *CapsCache {
	return &CapsCache{entries: make(map[string][]byte)}
}

func (c *CapsCache) Get(threadID string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	data, ok := c.entries[threadID]
	return data, ok
}

func (c *CapsCache) Set(threadID string, data []byte) {
	cp := make([]byte, len(data))
	copy(cp, data)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[threadID] = cp
}

func (c *CapsCache) Invalidate(threadID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, threadID)
}

// cachedQueryFn wraps an upstream daemon-query function so that
// get_active_caps responses are cached per thread. All other RPC
// methods pass through untouched — the cache is scoped to the one call
// that drives capability injection.
func cachedQueryFn(
	cache *CapsCache,
	threadID string,
	upstream func(method string, params map[string]any) (json.RawMessage, error),
) func(method string, params map[string]any) (json.RawMessage, error) {
	return func(method string, params map[string]any) (json.RawMessage, error) {
		if method != "get_active_caps" {
			return upstream(method, params)
		}
		if cached, ok := cache.Get(threadID); ok {
			return json.RawMessage(cached), nil
		}
		raw, err := upstream(method, params)
		if err == nil && len(raw) > 0 {
			cache.Set(threadID, []byte(raw))
		}
		return raw, err
	}
}

// invalidateThreadCaches drops all per-thread cache entries (frozenStubs +
// capsCache + briefingCache) for the given thread. The sawtooth refreeze path
// calls this so the next request rebuilds the frozen prefix, the capabilities
// rendering, and the briefing+codemap snapshot together — picking up any
// activate/deactivate changes that happened since the previous freeze.
// Other threads sharing this project stay untouched: their briefing snapshot
// survives because briefingCache is keyed by threadID.
func (s *Server) invalidateThreadCaches(threadID, project, projectDir, model string) {
	if s.frozenStubs != nil {
		s.frozenStubs.Invalidate(threadID)
	}
	if s.capsCache != nil {
		s.capsCache.Invalidate(threadID)
	}
	// Regenerating a briefing that the model's gates will never inject costs an
	// LLM refinement pass for nothing.
	if threadID != "" && project != "" && isFeatureEnabled(&s.cfg, model, "briefing") {
		s.refreshBriefing(threadID, project, projectDir)
	}
}

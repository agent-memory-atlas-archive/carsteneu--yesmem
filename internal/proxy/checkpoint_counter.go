package proxy

import (
	"log"
	"sync"
)

// tokenCheckpoints tracks, per thread, the conversation size at which an
// injectable hint last fired. Plan checkpoints and the docs hint share it so
// the collapse behaviour below exists in one place.
type tokenCheckpoints struct {
	mu             sync.Mutex
	lastTokenCount map[string]int // threadID → totalTokens at the last mark
}

// maxThreadStates bounds the injection-state maps of this package that are keyed
// by a thread id: the checkpoint counters here and the two rules maps
// (rules_inject.go). The id comes from a client header, so those maps are
// caller-writable and need a cap. What they hold is a baseline, not history: a
// dropped entry costs one delayed interval, so a random trim is enough and no
// per-entry clock is needed.
//
// The other per-thread maps of the server are capped by the same limit; see
// pruneThreadStateMaps in thread_state_prune.go, which runs on the periodic
// pruner and registers every map that carries this exposure.
const maxThreadStates = 5000

// pruneThreadStates trims a thread-keyed map down to keep entries. Map iteration
// order is randomized, so the survivors are arbitrary — see maxThreadStates for
// why that is acceptable.
func pruneThreadStates(m map[string]int, keep int) int {
	removed := 0
	for threadID := range m {
		if len(m) <= keep {
			break
		}
		delete(m, threadID)
		removed++
	}
	return removed
}

func newTokenCheckpoints() *tokenCheckpoints {
	return &tokenCheckpoints{lastTokenCount: make(map[string]int)}
}

// due reports whether interval tokens have accumulated since the thread's mark
// and moves the mark forward when they have.
//
// A context that shrank below the mark (collapse or stubbing) restarts the
// interval from the current size. Comparing deltas against a mark above the
// current size would otherwise keep the hint silent until the conversation
// climbs back over its pre-collapse high-water mark.
func (c *tokenCheckpoints) due(threadID string, totalTokens, interval int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.pruneLocked()

	last, seen := c.lastTokenCount[threadID]
	switch {
	case !seen:
		c.lastTokenCount[threadID] = totalTokens
		return false
	case totalTokens < last:
		c.lastTokenCount[threadID] = totalTokens
		return false
	case totalTokens-last < interval:
		return false
	}
	c.lastTokenCount[threadID] = totalTokens
	return true
}

// reset moves the mark to totalTokens without firing: a plan update makes the
// pending checkpoint stale, not due.
func (c *tokenCheckpoints) reset(threadID string, totalTokens int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.pruneLocked()
	c.lastTokenCount[threadID] = totalTokens
}

// pruneLocked trims the map after a write. Must be called with c.mu held.
func (c *tokenCheckpoints) pruneLocked() {
	if len(c.lastTokenCount) <= maxThreadStates {
		return
	}
	removed := pruneThreadStates(c.lastTokenCount, maxThreadStates*3/4)
	// Above the cap every thread can lose its baseline, so the event must be
	// visible in the log rather than only in the memory profile.
	log.Printf("[checkpoints] pruned %d idle thread states", removed)
}

// clear drops the thread's mark entirely (the plan is gone).
func (c *tokenCheckpoints) clear(threadID string) {
	c.mu.Lock()
	delete(c.lastTokenCount, threadID)
	c.mu.Unlock()
}

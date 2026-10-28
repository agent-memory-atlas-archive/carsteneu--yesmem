package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// maxCollapseBoundaries bounds the per-thread hold. Clearing the map only costs
// every thread one recomputed boundary, which is the old behaviour.
const maxCollapseBoundaries = 4096

// collapseBoundaries holds the collapse cutoff a thread settled on.
//
// The cutoff comes from a token budget whose estimate factor moves as API
// actuals arrive, so recomputing it per request can move the boundary even
// though the client history behind it did not change (YM-411). A boundary that
// walks backwards re-opens a tool pair the previous request had already
// collapsed; validateToolPairs then removes that orphan again. Holding the
// decision keeps the payload byte-stable while the history under it is stable.
//
// The held value only ever moves forward: a larger freshly computed cutoff is
// taken over, because that is the estimate asking for more collapse. Holding a
// smaller one back would leave the payload above the budget the collapse exists
// to reach. For the same reason the hold is not dropped when the frozen prefix
// goes away: a re-stub cycle is exactly where the drift would otherwise move the
// boundary backwards, and over-collapsing (the only direction the hold can err
// in) is the documented trade-off of this path.
type collapseBoundaries struct {
	mu      sync.Mutex
	entries map[string]collapseBoundary
}

type collapseBoundary struct {
	cutoff int
	anchor string // stable hash of messages[cutoff-1]
}

func newCollapseBoundaries() *collapseBoundaries {
	return &collapseBoundaries{entries: make(map[string]collapseBoundary)}
}

// resolve returns the cutoff for this request and whether a held value was
// reused. recompute runs once, before the hold is consulted, because the fresh
// value decides whether the hold is still sufficient.
func (b *collapseBoundaries) resolve(threadID string, messages []any, recompute func() int) (int, bool) {
	fresh := recompute()
	if threadID == "" || fresh <= 0 {
		return fresh, false
	}

	// The anchor hashes one client-supplied message, and that message can be a
	// large tool result. It is computed outside the mutex: the hold is advisory,
	// its worst case is a recomputed cutoff, while hashing under the mutex would
	// serialize every thread's boundary decision behind one large request.
	b.mu.Lock()
	held, ok := b.entries[threadID]
	b.mu.Unlock()

	if ok && held.cutoff > 0 && held.cutoff < len(messages) && held.anchor != "" &&
		held.anchor == collapseAnchor(messages, held.cutoff) {
		if fresh > held.cutoff {
			b.store(threadID, fresh, collapseAnchor(messages, fresh))
			return fresh, false
		}
		return held.cutoff, true
	}

	b.store(threadID, fresh, collapseAnchor(messages, fresh))
	return fresh, false
}

func (b *collapseBoundaries) store(threadID string, cutoff int, anchor string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) >= maxCollapseBoundaries {
		b.entries = make(map[string]collapseBoundary)
	}
	b.entries[threadID] = collapseBoundary{cutoff: cutoff, anchor: anchor}
}

// collapseAnchor identifies the message a boundary sits behind. A client that
// rewrites history changes that message and the boundary is recomputed; an
// append-only history leaves it untouched and the hold stands.
func collapseAnchor(messages []any, cutoff int) string {
	if cutoff <= 0 || cutoff > len(messages) {
		return ""
	}
	data, err := json.Marshal(messages[cutoff-1])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// collapseCutoffFor returns the collapse cutoff for this request. On the
// sawtooth path the decision is held per thread, because that path invalidates
// the hold together with the frozen prefix. The legacy path recomputes it: it
// has no such invalidation, so a hold would outlive every reason for it.
func (s *Server) collapseCutoffFor(threadID string, messages []any, contentBudget int, estimateFn TokenEstimateFunc) (int, bool) {
	recompute := func() int {
		return CalcCollapseCutoff(messages, s.cfg.KeepRecent, contentBudget, estimateFn)
	}
	if !s.cfg.SawtoothEnabled || s.collapseBoundaries == nil {
		return recompute(), false
	}
	return s.collapseBoundaries.resolve(threadID, messages, recompute)
}

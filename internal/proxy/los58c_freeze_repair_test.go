package proxy

import (
	"fmt"
	"io"
	"log"
	"testing"
)

// YM-411: the frozen prefix must carry the pair-repaired collapse result.
//
// The repair used to run only on the outgoing request, after the same array had
// already been frozen. Every later request rebuilt its prefix from the unfixed
// copy, so validateToolPairs removed the same orphan again: the audit measured
// 50 repairs in 49 log lines at one unchanged boundary.
func TestFrozenPrefixStoresPairRepairedMessages(t *testing.T) {
	s := &Server{
		logger:      log.New(io.Discard, "", 0),
		frozenStubs: NewFrozenStubs(),
	}

	collapsed := []any{
		map[string]any{"role": "user", "content": "-"},
		map[string]any{"role": "user", "content": "[Archiv: Messages 1-443 (443 msgs)]"},
		// The tool_use of this result sits before the collapse boundary and was
		// archived, so the block is an orphan that Pass 2 has to remove.
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_archived", "content": "output"},
		}},
		map[string]any{"role": "assistant", "content": "weiter"},
	}

	req := map[string]any{"messages": collapsed}
	s.freezeStubsAndInjectBreakpoint(req, "tid-ym411", len(collapsed), collapsed[len(collapsed)-1], 100, 100)

	// The store validates against the current history length, so the reader
	// passes an array of the pre-repair size, like the next request would.
	frozen := s.frozenStubs.Get("tid-ym411", make([]any, len(collapsed)))
	if frozen == nil {
		t.Fatal("frozen prefix missing after freeze")
	}
	if _, orphans := validateToolPairs(frozen.Messages, nil); orphans != 0 {
		t.Fatalf("frozen prefix still carries %d orphaned tool_result(s): the repair must run before the freeze", orphans)
	}
}

// The stored token estimate belongs to the bytes that were stored, not to the
// array the caller measured before the repair.
func TestFrozenPrefixEstimateFollowsTheRepair(t *testing.T) {
	s := &Server{
		logger:      log.New(io.Discard, "", 0),
		frozenStubs: NewFrozenStubs(),
	}
	orphan := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_archived", "content": "a long orphaned output"},
	}}
	collapsed := []any{
		map[string]any{"role": "user", "content": "-"},
		map[string]any{"role": "assistant", "content": "weiter"},
		orphan,
	}
	req := map[string]any{"messages": collapsed}
	before := s.countMessageTokens(collapsed)
	s.freezeStubsAndInjectBreakpoint(req, "tid-est", len(collapsed), collapsed[len(collapsed)-1], before, before)

	frozen := s.frozenStubs.Get("tid-est", make([]any, len(collapsed)))
	if frozen == nil {
		t.Fatal("frozen prefix missing after freeze")
	}
	if frozen.Tokens >= before {
		t.Fatalf("stored estimate %d not below the pre-repair %d: the removed orphan is still counted", frozen.Tokens, before)
	}
}

func boundaryTestMessages(n int) []any {
	msgs := make([]any, n)
	for i := range msgs {
		msgs[i] = map[string]any{"role": "user", "content": fmt.Sprintf("msg-%d", i)}
	}
	return msgs
}

// YM-411: the cutoff a thread settled on survives a drifting estimate. A larger
// budget asks to keep more history, i.e. it moves the cutoff backwards, which
// re-opens the tool pair the previous request had collapsed.
func TestCollapseBoundariesHoldAgainstBackwardDrift(t *testing.T) {
	b := newCollapseBoundaries()
	msgs := boundaryTestMessages(200)

	if got, held := b.resolve("tid", msgs, func() int { return 160 }); got != 160 || held {
		t.Fatalf("first resolve = (%d, %t), want (160, false)", got, held)
	}
	if got, held := b.resolve("tid", msgs, func() int { return 150 }); got != 160 || !held {
		t.Fatalf("backward drift = (%d, %t), want (160, true)", got, held)
	}
}

// YM-411: a smaller budget asks for less history kept, i.e. a larger cutoff, and
// that move is taken over. Otherwise the hold would keep the payload above the
// budget the collapse exists to reach.
func TestCollapseBoundariesTakeForwardCutoff(t *testing.T) {
	b := newCollapseBoundaries()
	msgs := boundaryTestMessages(200)

	b.resolve("tid", msgs, func() int { return 160 })
	if got, held := b.resolve("tid", msgs, func() int { return 172 }); got != 172 || held {
		t.Fatalf("forward cutoff = (%d, %t), want (172, false)", got, held)
	}
	if got, held := b.resolve("tid", msgs, func() int { return 165 }); got != 172 || !held {
		t.Fatalf("after forward cutoff = (%d, %t), want (172, true)", got, held)
	}
}

// YM-411: a rewritten history invalidates the hold. An insertion before the
// boundary shifts every later message, so the anchor no longer matches even
// though the boundary index is unchanged.
func TestCollapseBoundariesResetOnRewrittenHistory(t *testing.T) {
	b := newCollapseBoundaries()
	msgs := boundaryTestMessages(200)

	b.resolve("tid", msgs, func() int { return 100 })

	msgs[99] = map[string]any{"role": "user", "content": "rewritten"}
	if got, held := b.resolve("tid", msgs, func() int { return 90 }); got != 90 || held {
		t.Fatalf("after rewrite = (%d, %t), want (90, false)", got, held)
	}

	b.resolve("tid2", msgs, func() int { return 100 })
	shifted := append([]any{map[string]any{"role": "user", "content": "new first turn"}}, msgs...)
	if got, held := b.resolve("tid2", shifted, func() int { return 90 }); got != 90 || held {
		t.Fatalf("after insertion = (%d, %t), want (90, false)", got, held)
	}
}

// YM-411: the sawtooth path re-stubs after the frozen prefix expires, and that
// is exactly the turn where a drifting estimate would move the boundary
// backwards. The hold must survive that invalidation.
func TestCollapseCutoffHoldSurvivesPrefixInvalidation(t *testing.T) {
	s := &Server{
		cfg:                Config{SawtoothEnabled: true, KeepRecent: 5},
		logger:             log.New(io.Discard, "", 0),
		frozenStubs:        NewFrozenStubs(),
		collapseBoundaries: newCollapseBoundaries(),
	}
	msgs := boundaryTestMessages(200)
	estimate := TokenEstimateFunc(func(string) int { return 10 })

	first, held := s.collapseCutoffFor("tid", msgs, 400, estimate)
	if first == 0 || held {
		t.Fatalf("first cutoff = (%d, %t), want a fresh non-zero decision", first, held)
	}

	s.invalidateThreadCaches("tid", "", "", "")

	// A larger budget asks to keep more history: the fresh cutoff would move
	// backwards, which is the move the hold refuses.
	got, held := s.collapseCutoffFor("tid", msgs, 600, estimate)
	if !held || got != first {
		t.Fatalf("after invalidation = (%d, %t), want the held %d", got, held, first)
	}
}

// YM-411: the legacy path has no prefix invalidation, so it must not hold.
func TestCollapseCutoffNotHeldWithoutSawtooth(t *testing.T) {
	s := &Server{cfg: Config{SawtoothEnabled: false}, collapseBoundaries: newCollapseBoundaries()}
	msgs := boundaryTestMessages(200)

	estimate := TokenEstimateFunc(func(string) int { return 10 })
	if _, held := s.collapseCutoffFor("tid", msgs, 500, estimate); held {
		t.Fatal("legacy path must recompute the cutoff per request")
	}
}

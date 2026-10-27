package proxy

import (
	"fmt"
	"io"
	"log"
	"testing"
)

// los11bThreadCap is the contract the pruning tests assert: no per-thread
// injection map may grow past this many entries, whatever the constant in the
// implementation is called.
const los11bThreadCap = 5000

// YM-252 (PRX-P29): the per-thread injection state is keyed by an id the client
// sends in a header, and nothing ever dropped an entry, so a caller could grow
// these maps without bound. The cap is enforced in the write path, so an
// ordinary request never pays more than a length check.
func TestTokenCheckpoints_PrunesThreadMap_Los11b(t *testing.T) {
	c := newTokenCheckpoints()
	for i := 0; i <= los11bThreadCap; i++ {
		c.due(fmt.Sprintf("ses-%d", i), 1000, 10000)
	}

	c.mu.Lock()
	size := len(c.lastTokenCount)
	c.mu.Unlock()

	if size > los11bThreadCap {
		t.Fatalf("tokenCheckpoints kept %d entries, cap is %d", size, los11bThreadCap)
	}
	if size == 0 {
		t.Fatal("prune emptied the map instead of trimming it")
	}
}

// Rules injection keeps two maps keyed by the same thread id; both have to be
// bounded, and a pruned thread must still be served on its next request.
func TestRulesInject_PrunesThreadMap_Los11b(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0)}

	for i := 0; i <= los11bThreadCap; i++ {
		s.rulesInject(fmt.Sprintf("ses-%d", i), 0, "")
	}

	s.rulesMu.Lock()
	tokens := len(s.rulesTokenCount)
	collapse := len(s.rulesCollapseInjected)
	s.rulesMu.Unlock()

	if tokens > los11bThreadCap {
		t.Fatalf("rulesTokenCount kept %d entries, cap is %d", tokens, los11bThreadCap)
	}
	if collapse > los11bThreadCap {
		t.Fatalf("rulesCollapseInjected kept %d entries, cap is %d", collapse, los11bThreadCap)
	}

	// The pruned thread is not broken: it re-baselines instead of panicking.
	if got := s.rulesInject("ses-late", 0, ""); got != "" {
		t.Fatalf("first call for an unseeded thread must not inject, got %q", got)
	}
}

// The docs hint shares the checkpoint counter, so the same cap must hold there.
func TestDocsHint_PrunesThreadMap_Los11b(t *testing.T) {
	// docsCp is package state shared with docs_inject_test.go: hand it back
	// empty so the 5001 probe threads cannot influence another test.
	defer func() {
		docsCp.mu.Lock()
		docsCp.lastTokenCount = make(map[string]int)
		docsCp.mu.Unlock()
	}()

	for i := 0; i <= los11bThreadCap; i++ {
		docsCp.due(fmt.Sprintf("ses-%d", i), 1000, docsHintInterval)
	}

	docsCp.mu.Lock()
	size := len(docsCp.lastTokenCount)
	docsCp.mu.Unlock()

	if size > los11bThreadCap {
		t.Fatalf("docsCp kept %d entries, cap is %d", size, los11bThreadCap)
	}
}

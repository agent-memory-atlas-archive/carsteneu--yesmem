package proxy

import (
	"fmt"
	"testing"
)

// YM-406 (PRX-P54, 4): the periodic pruner dropped arbitrary entries from the
// per-thread maps. For loopStates that means losing a pending escalation: the
// ladder restarts, and an agent stuck in the same loop receives the same first
// warning again instead of the next level.
//
// The ratio here is chosen so that the removal set exceeds the pool of states
// WITHOUT escalation (4000 plain, 1500 escalated, 5000 kept). The property is
// then observable without relying on map iteration order: the escalated states
// must survive, because there are enough plain candidates to remove.
func TestLos57bPruneKeepsEscalatedLoopStates(t *testing.T) {
	plain := 4000
	escalated := 1500

	s := &Server{loopStates: make(map[string]*LoopState, plain+escalated)}
	for i := 0; i < plain; i++ {
		s.loopStates[fmt.Sprintf("plain-%d", i)] = &LoopState{}
	}
	for i := 0; i < escalated; i++ {
		s.loopStates[fmt.Sprintf("escalated-%d", i)] = &LoopState{WarningCount: 2}
	}

	s.pruneThreadStateMaps()

	if got := len(s.loopStates); got > maxThreadStates {
		t.Errorf("loopStates has %d entries after the prune, cap is %d", got, maxThreadStates)
	}
	lost := 0
	for i := 0; i < escalated; i++ {
		if _, ok := s.loopStates[fmt.Sprintf("escalated-%d", i)]; !ok {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("the prune dropped %d of %d loop states with a pending escalation", lost, escalated)
	}
}

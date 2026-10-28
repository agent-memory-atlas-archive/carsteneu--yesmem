package proxy

import (
	"bytes"
	"log"
	"testing"
)

// YM-275(1): the decay age was computed from the proxy-wide request counter, so
// a quiet thread's stubs aged with every other thread's traffic. Ten loops run
// in parallel here; a session that pauses while the others work came back to
// stage-3 stubs, i.e. to an empty history. The age has to follow the thread's
// own turns.
func TestLos56c_DecayAgeFollowsTheThreadsOwnTurns(t *testing.T) {
	server := &Server{
		decayTrackers: make(map[string]*DecayTracker),
		logger:        log.New(&bytes.Buffer{}, "", 0),
	}

	quiet := server.decayFor("los56c-quiet")
	busy := server.decayFor("los56c-busy")

	first := quiet.Tick()
	quiet.MarkStubbed(5, first, 0)

	// Another thread makes a hundred requests while this one is silent.
	for i := 0; i < 100; i++ {
		busy.Tick()
	}

	second := quiet.Tick()
	if second != first+1 {
		t.Fatalf("thread clock advanced by %d for one own request, want 1", second-first)
	}
	if stage := quiet.GetStage(5, second, 100, 2.5); stage != DecayStage0 {
		t.Errorf("stub decayed to stage %d although this thread made one request; foreign traffic must not age it", stage)
	}
	// A hundred own requests must still decay it, otherwise the fix would have
	// disabled decay instead of scoping it.
	for quiet.Tick() < 120 {
	}
	if stage := quiet.GetStage(5, quiet.Now(), 100, 2.5); stage != DecayStage3 {
		t.Errorf("stub stayed at stage %d after 100 own requests, want stage 3", stage)
	}
}

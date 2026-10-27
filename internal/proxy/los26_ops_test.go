package proxy

import "testing"

// shouldStub's hysteresis latch (s.stubActive) is one process-wide flag, so the
// legacy gate resolves the process-wide threshold on purpose. Feeding it a
// per-thread threshold would let one session toggle stubbing for another: the
// latch set by a session with a low override makes a different session stub
// below its own threshold. Session overrides therefore apply to the sawtooth
// path and the stub cycle, and this gate stays process-wide until the latch
// itself becomes per-thread.
func TestLos26_ShouldStubStaysProcessWide(t *testing.T) {
	s := &Server{cfg: Config{TokenThreshold: 200000, TokenMinimumThreshold: 150000}}
	s.setThreadTokenThresholdOverride("thread-a", 100000)

	if s.shouldStub(110000, "") {
		t.Error("shouldStub applied the session threshold 100000 at 110000 tokens; the hysteresis latch is process-wide")
	}
	if !s.shouldStub(210000, "") {
		t.Error("shouldStub did not activate above the process-wide threshold 200000")
	}
}

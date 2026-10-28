package proxy

import "testing"

// TestAssociativeContextSendsSessionWithSkipRecentLos10c (YM-305 [MEM-F37]): the
// daemon can only scope skip_recent to the caller's own session when the proxy
// names it. Without the id the daemon falls back to a global window over the
// newest learnings, which any other session's inserts can shift.
func TestAssociativeContextSendsSessionWithSkipRecentLos10c(t *testing.T) {
	paramsCh := make(chan map[string]any, 1)
	s := fakeDaemon(t, func(method string, params map[string]any) string {
		if method == "hybrid_search" {
			select {
			case paramsCh <- params:
			default:
			}
			return `{"results":[]}`
		}
		return "null"
	})
	s.lastInjectedIDs = make(map[string]map[int64]string)
	s.sessionInjectCounts = make(map[string]map[int64]int)
	s.lastTurnInjected = make(map[string]map[int64]bool)

	s.findAssociativeContextFor(
		"bitte pruefe die sache mit dem deployment",
		"/home/user/proj",
		"sess-los10c-proxy-1",
		nil,
	)

	var got map[string]any
	select {
	case got = <-paramsCh:
	default:
		t.Fatal("hybrid_search was not called")
	}
	if got["skip_recent"] != float64(3) {
		t.Errorf("skip_recent = %v, want 3", got["skip_recent"])
	}
	if got["session_id"] != "sess-los10c-proxy-1" {
		t.Errorf("session_id = %v, want the thread id so the daemon can scope the skip window", got["session_id"])
	}
}

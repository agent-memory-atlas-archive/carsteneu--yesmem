package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// los26cFakeDaemon serves the daemon socket protocol and counts the requests it
// receives. Every get_proxy_state answer is an empty value, so the caller keeps
// looking for an override.
func los26cFakeDaemon(t *testing.T, dir string) *int32 {
	t.Helper()
	sock := filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var count int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				atomic.AddInt32(&count, 1)
				c.SetDeadline(time.Now().Add(2 * time.Second))
				json.NewEncoder(c).Encode(map[string]any{"result": map[string]any{"value": ""}})
			}(conn)
		}
	}()
	return &count
}

// TestLos26c_SessionOverrideRefreshHasGlobalBudget pins finding 6 of YM-315.
// The refresh window is keyed by threadID, and the thread id comes from the
// client. A caller that invents one id per request therefore paid one daemon
// round trip per request (two, when the id carries a backend prefix) and never
// waited. The window alone bounds nothing; the process needs a budget.
func TestLos26c_SessionOverrideRefreshHasGlobalBudget(t *testing.T) {
	dir := shortSocketDir(t)
	count := los26cFakeDaemon(t, dir)

	s := &Server{
		cfg:                    Config{DataDir: dir},
		logger:                 log.New(io.Discard, "", 0),
		sessionOverrideRefresh: map[string]time.Time{},
	}
	s.daemonReady.Store(true) // no cold-start retry loop in the test

	// Twice the budget in fresh thread ids: a per-thread window lets every one
	// of them through.
	requests := sessionOverrideRefreshBudget * 2
	for i := 0; i < requests; i++ {
		s.refreshSessionOverride(fmt.Sprintf("client-thread-%d", i))
	}

	got := int(atomic.LoadInt32(count))
	if got == 0 {
		t.Fatal("the refresh path did not reach the daemon at all")
	}
	if got > sessionOverrideRefreshBudget {
		t.Fatalf("daemon reads = %d for %d distinct thread ids, budget is %d: the per-thread window is not a bound",
			got, requests, sessionOverrideRefreshBudget)
	}
}

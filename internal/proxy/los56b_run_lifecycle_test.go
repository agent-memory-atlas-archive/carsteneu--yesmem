package proxy

import (
	"net"
	"strings"
	"testing"
	"time"
)

// YM-399 (PRX-P51): Run held two process-lifetime resources and both outlived
// the server. The shutdown waiter blocked on the signal channel forever when
// Run returned for any other reason (a listen error is the ordinary case), and
// the signal.Notify registration was never released, so the os/signal loop
// goroutine stayed for the rest of the process as well. Tests ended a proxy by
// starting Run and walking away, which is why the package leak guard had to
// tolerate two of them.

// waitForRunEnd waits for Run to return and reports a non-nil error.
func waitForRunEnd(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("runUntil returned %v, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runUntil did not return after the stop channel was closed")
	}
}

// waitForWaiters waits until every shutdown goroutine has ended. The waiter
// finishes its Shutdown after Run returned, so this polls instead of asserting
// immediately.
func waitForWaiters(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := liveRunWaiters.Load(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("live run waiters = %d, want %d", liveRunWaiters.Load(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// freePort returns an address that was free a moment ago. The window between
// closing and reusing it is what the two tests below need.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// proxyTestConfig is the smallest runnable configuration: no auto-discovery, no
// fork traffic, a data directory of its own. A configuration with an occupied
// address must fail instead of hanging, which the last test below checks.
func proxyTestConfig(addr string) Config {
	return Config{
		ListenAddr:            addr,
		TargetURL:             "http://127.0.0.1:1",
		OpenAITargetURL:       "http://127.0.0.1:1",
		TokenThreshold:        250000,
		TokenMinimumThreshold: 100000,
		KeepRecent:            10,
	}
}

// startProxyForTest starts runUntil and ends it through the stop channel when
// the test finishes. No signal is sent: SIGTERM would reach every handler in the
// test process, not only this server.
func startProxyForTest(t *testing.T, cfg Config) {
	t.Helper()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- runUntil(cfg, stop) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("tcp", cfg.ListenAddr)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			close(stop)
			t.Fatalf("proxy did not start listening on %s", cfg.ListenAddr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		close(stop)
		waitForRunEnd(t, errCh)
	})
}

// A listen error is the ordinary early return: another process holds the port.
// The waiter goroutine and the status writer must both be gone afterwards.
//
// The os/signal loop goroutine is deliberately not asserted: Go starts it once
// per process on the first Notify and does not end it on Stop (measured on
// go1.25), so its presence says nothing about this registration. The release
// itself is the deferred signal.Stop in runUntil.
func TestLos56bRunReleasesItsWaiterOnListenError(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer held.Close()

	cfg := proxyTestConfig(held.Addr().String())
	cfg.DataDir = t.TempDir()

	baselineWriters := liveCacheStatusWriters.Load()
	baselineWaiters := liveRunWaiters.Load()

	if err := runUntil(cfg, nil); err == nil {
		t.Fatal("runUntil returned nil on an occupied listen address")
	}

	waitForWaiters(t, baselineWaiters)
	if got := liveCacheStatusWriters.Load(); got != baselineWriters {
		t.Errorf("live cache status writers = %d, want the baseline %d", got, baselineWriters)
	}
}

// A stop request ends the server completely: Run returns without an error, the
// listener is free for the next bind, and neither the writer nor the waiter
// survives it.
func TestLos56bRunStopsAndReleasesItsListener(t *testing.T) {
	addr := freePort(t)
	cfg := proxyTestConfig(addr)
	cfg.DataDir = t.TempDir()

	baselineWriters := liveCacheStatusWriters.Load()
	baselineWaiters := liveRunWaiters.Load()

	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- runUntil(cfg, stop) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy did not start listening on %s", addr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The writer runs while the server serves.
	if got := liveCacheStatusWriters.Load(); got != baselineWriters+1 {
		t.Fatalf("live cache status writers = %d, want %d while the proxy serves", got, baselineWriters+1)
	}

	close(stop)
	waitForRunEnd(t, errCh)
	waitForWaiters(t, baselineWaiters)

	if got := liveCacheStatusWriters.Load(); got != baselineWriters {
		t.Errorf("live cache status writers = %d, want the baseline %d after shutdown", got, baselineWriters)
	}

	// The listener must be gone, not merely quiet: a second server on the same
	// address is the proof.
	rebound, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the listen address %s is still held after shutdown: %v", addr, err)
	}
	rebound.Close()
}

// The waiter must also end when the stop channel is never closed and no signal
// arrives: Run's own return releases it.
func TestLos56bRunWaiterDoesNotOutliveAReturn(t *testing.T) {
	baselineWaiters := liveRunWaiters.Load()

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer held.Close()

	cfg := proxyTestConfig(held.Addr().String())
	cfg.DataDir = t.TempDir()
	if err := runUntil(cfg, nil); err == nil {
		t.Fatal("runUntil returned nil on an occupied listen address")
	}

	waitForWaiters(t, baselineWaiters)

	// A second run reuses the released registration slot; without the release
	// the counter above would already have caught it, and this line only proves
	// that the address is still usable.
	addr := freePort(t)
	second := proxyTestConfig(addr)
	second.DataDir = t.TempDir()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- runUntil(second, stop) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, dialErr := net.Dial("tcp", addr)
		if dialErr == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second proxy did not start listening on %s", addr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	waitForRunEnd(t, errCh)
	waitForWaiters(t, baselineWaiters)
}

// An address that cannot be bound must surface as an error that names it, not
// as a hanging test.
func TestLos56bProxyTestConfigReportsTheListenAddress(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer held.Close()

	cfg := proxyTestConfig(held.Addr().String())
	cfg.DataDir = t.TempDir()
	runErr := runUntil(cfg, nil)
	if runErr == nil {
		t.Fatal("runUntil returned nil on an occupied listen address")
	}
	if !strings.Contains(runErr.Error(), held.Addr().String()) {
		t.Fatalf("listen error %q does not name the address %q", runErr.Error(), held.Addr().String())
	}
}

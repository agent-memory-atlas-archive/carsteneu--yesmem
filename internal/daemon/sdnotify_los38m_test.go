package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// notifyListener stands in for systemd's notification socket.
func notifyListener(t *testing.T) (*net.UnixConn, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, path
}

func readDatagram(t *testing.T, conn *net.UnixConn, timeout time.Duration) (string, bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 256)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		return "", false
	}
	return string(buf[:n]), true
}

// YM-312: the daemon must report READY so systemd's Type=notify start finishes,
// and it must keep proving liveness afterwards.
func TestSendNotifyStateReachesTheSystemdSocket(t *testing.T) {
	conn, path := notifyListener(t)
	t.Setenv("NOTIFY_SOCKET", path)

	if err := sendNotifyState("READY=1"); err != nil {
		t.Fatalf("sendNotifyState: %v", err)
	}
	got, ok := readDatagram(t, conn, 2*time.Second)
	if !ok {
		t.Fatal("systemd socket received no datagram")
	}
	if got != "READY=1" {
		t.Fatalf("datagram = %q, want READY=1", got)
	}
}

// TestNotifyWithoutSystemdIsSilent: started by hand there is no NOTIFY_SOCKET, and
// the daemon must not try to reach one.
func TestNotifyWithoutSystemdIsSilent(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if _, ok := notifyAddr(); ok {
		t.Fatal("notifyAddr reported a socket without NOTIFY_SOCKET")
	}
	if err := sendNotifyState("READY=1"); err != nil {
		t.Fatalf("sendNotifyState without a socket must be a no-op, got %v", err)
	}
}

// startWatchdog runs the loop in the background and returns a stop function that
// cancels it and waits for the goroutine to end. Joining matters: a callback that
// reports after the test finished is a log line out of nowhere.
func startWatchdog(t *testing.T, interval time.Duration, probe func() error, send func(string) error) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDaemonWatchdog(ctx, interval, probe, send)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("the watchdog loop kept running after cancellation")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// TestWatchdogToleratesOneMissedProbeButThenGoesQuiet: a dead socket must silence
// the daemon so systemd restarts it, but a single timed-out dial (busy backlog,
// nobody else's fault) must not cost every running agent a restart.
//
// The counter is atomic and the loop is observed through a deadline: a fixed
// sleep was too short under load, which made this test flaky (YM-341).
func TestWatchdogPingsOnlyAfterTheSocketAnswered(t *testing.T) {
	const interval = 5 * time.Millisecond

	var failedPings atomic.Int32
	stop := startWatchdog(t, interval,
		func() error { return os.ErrDeadlineExceeded },
		func(string) error { failedPings.Add(1); return nil })
	waitForCount(t, &failedPings, daemonWatchdogProbeTolerance, interval)
	// The tolerated misses are in. Give the loop several more ticks to prove it
	// stays quiet instead of pinging again.
	time.Sleep(10 * interval)
	if got := int(failedPings.Load()); got != daemonWatchdogProbeTolerance {
		t.Fatalf("%d pings with a dead socket, want exactly %d tolerated misses before the loop goes quiet",
			got, daemonWatchdogProbeTolerance)
	}
	stop()

	// Working probe: pings keep coming.
	var pings atomic.Int32
	var wrongState atomic.Value // string, recorded instead of reported from the goroutine
	startWatchdog(t, interval,
		func() error { return nil },
		func(state string) error {
			if state != "WATCHDOG=1" {
				wrongState.Store(state)
			}
			pings.Add(1)
			return nil
		})
	waitForCount(t, &pings, 3, interval)
	if got := int(pings.Load()); got < 3 {
		t.Fatalf("only %d watchdog pings in a running loop", got)
	}
	if bad, ok := wrongState.Load().(string); ok {
		t.Errorf("state = %q, want WATCHDOG=1", bad)
	}
}

// waitForCount blocks until the counter reaches want, so a loaded host gets as
// long as it needs without turning a slow tick into a failure.
func waitForCount(t *testing.T, counter *atomic.Int32, want int, interval time.Duration) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for counter.Load() < int32(want) && time.Now().Before(deadline) {
		time.Sleep(interval)
	}
	if got := int(counter.Load()); got < want {
		t.Fatalf("counter reached %d after 2s, want %d", got, want)
	}
}

// TestStripNotifyVarsKeepsTheAddressOutOfChildEnvironments: systemd hands the
// supervisor channel to the daemon, and whatever stays in its environment reaches
// every spawned agent and sandbox job.
func TestStripNotifyVarsKeepsTheAddressOutOfChildEnvironments(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/run/user/1000/systemd/notify")
	t.Setenv("WATCHDOG_USEC", "90000000")
	t.Setenv("WATCHDOG_PID", "4242")

	stripNotifyVars()

	for _, key := range []string{"NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID"} {
		if value := os.Getenv(key); value != "" {
			t.Errorf("%s survived the strip (%q) and would reach every child process", key, value)
		}
	}
}

// TestWatchdogStopsOnContextCancellation: the loop must end with the daemon, or a
// restart leaves a stray pinger behind that keeps systemd satisfied.
func TestWatchdogStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runDaemonWatchdog(ctx, time.Millisecond, func() error { return nil }, func(string) error { return nil })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog loop kept running after cancellation")
	}
}

// TestWatchdogIntervalFitsTheUnitDeadline pins the cross-file arithmetic: the unit
// ships WatchdogSec=90, and three pings must fit inside it.
func TestWatchdogIntervalFitsTheUnitDeadline(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join("..", "setup", "setup.go"))
	if err != nil {
		t.Skipf("setup.go not readable from here: %v", err)
	}
	if !strings.Contains(string(unit), "WatchdogSec=90") {
		t.Skip("unit deadline moved; adjust this test together with the unit")
	}
	if 3*daemonWatchdogInterval > 90*time.Second {
		t.Fatalf("daemonWatchdogInterval=%s leaves room for fewer than three pings in the 90s deadline", daemonWatchdogInterval)
	}
}

// TestWatchdogSurvivesATransientSendFailure: one failed write to the notify socket
// (EAGAIN on a full buffer, a brief manager stall) must not silence the daemon for
// good — that would let systemd kill a healthy daemon 90s later.
func TestWatchdogSurvivesATransientSendFailure(t *testing.T) {
	const interval = 5 * time.Millisecond

	var sends atomic.Int32
	startWatchdog(t, interval,
		func() error { return nil },
		func(string) error {
			if sends.Add(1) == 1 {
				return os.ErrClosed
			}
			return nil
		})
	waitForCount(t, &sends, 3, interval)
	if got := int(sends.Load()); got < 3 {
		t.Fatalf("the loop stopped after %d sends; a single failed write killed the watchdog", got)
	}
}

// TestWatchdogIntervalFollowsTheUnitsDeadline: the unit's WatchdogSec is the
// authority. A hand-tuned shorter deadline (WatchdogSec=30) would otherwise be
// outrun by a fixed 30s ping interval and turn into a restart loop.
func TestWatchdogIntervalFollowsTheUnitsDeadline(t *testing.T) {
	cases := []struct {
		name string
		usec string
		want time.Duration
	}{
		{"unit default 90s", "90000000", 30 * time.Second},
		{"hand-tuned 30s", "30000000", 10 * time.Second},
		{"absurdly small", "3000", time.Second},
		{"zero falls back", "0", daemonWatchdogInterval},
		{"negative falls back", "-9000", daemonWatchdogInterval},
		{"unset falls back", "", daemonWatchdogInterval},
		{"garbage falls back", "not-a-number", daemonWatchdogInterval},
	}
	for _, tc := range cases {
		t.Setenv("WATCHDOG_USEC", tc.usec)
		if got := watchdogInterval(); got != tc.want {
			t.Errorf("%s: watchdogInterval = %s, want %s", tc.name, got, tc.want)
		}
	}
}

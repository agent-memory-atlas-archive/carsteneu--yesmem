package daemon

import (
	"context"
	"net"
	"os"
	"strconv"
	"time"
)

// daemonWatchdogInterval is how often the daemon proves it is alive to systemd.
// It must stay well inside the unit's WatchdogSec (currently 90s), so a single
// missed ping is not a restart: three pings fit in the window.
const daemonWatchdogInterval = 30 * time.Second

// daemonWatchdogProbeTolerance is how many consecutive failed socket probes still
// ping. The notify socket is world-writable inside the user's own runtime and a
// busy accept backlog makes one dial time out; a restart costs every running agent,
// so one lost probe must not be enough. From the third consecutive failure the
// daemon goes quiet and systemd restarts it.
const daemonWatchdogProbeTolerance = 2

// watchdogInterval is how often the daemon proves it is alive. The unit's
// WatchdogSec is the authority: systemd exports it as WATCHDOG_USEC, and a third of
// it keeps two pings inside the deadline. The fallback covers a hand-written unit
// that sets WatchdogSec without the variable (and a daemon started by hand).
func watchdogInterval() time.Duration {
	raw := os.Getenv("WATCHDOG_USEC")
	usec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || usec <= 0 {
		return daemonWatchdogInterval
	}
	// Clamp before the conversion: a usec value near 2^63 would overflow the
	// duration and turn the interval negative, which stops the loop silently.
	const maxWatchdogUsec = int64(time.Hour / time.Microsecond)
	if usec > maxWatchdogUsec {
		usec = maxWatchdogUsec
	}
	interval := time.Duration(usec/3) * time.Microsecond
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

// notifyAddr returns the systemd notification socket, if the daemon was started
// by systemd with Type=notify. An abstract socket arrives as "@name" and needs no
// translation: net.Dial understands that form (verified on this host).
func notifyAddr() (string, bool) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return "", false
	}
	return addr, true
}

// sendNotifyTo sends one sd_notify datagram to a known address. Best effort by
// construction: a missing or unreadable socket means "not under systemd", and the
// daemon must run the same with or without a supervisor.
func sendNotifyTo(addr, state string) error {
	conn, err := net.Dial("unixgram", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// sendNotifyState resolves the address and sends. The daemon itself captures the
// address once at startup (see stripNotifyVars) and uses sendNotifyTo.
func sendNotifyState(state string) error {
	addr, ok := notifyAddr()
	if !ok {
		return nil
	}
	return sendNotifyTo(addr, state)
}

// stripNotifyVars removes the systemd notification variables from this process's
// environment after they were read once. Left in place they reach every spawned
// agent and sandbox job; NotifyAccess=main makes systemd drop their datagrams, but
// a child's environment is the wrong place for the supervisor channel either way.
func stripNotifyVars() {
	for _, key := range []string{"NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID"} {
		os.Unsetenv(key)
	}
}

// socketAnswers probes whether the daemon still accepts connections on its own
// socket. This is the liveness the watchdog reports: a process that lives on while
// its socket is dead is exactly the state Restart=always cannot see.
func socketAnswers(socketPath string) error {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// startDaemonWatchdog runs the watchdog loop next to the daemon: one ping per
// interval, and only after the socket answered. A wedged daemon therefore stops
// pinging and systemd restarts it instead of waiting for a timeout nobody reads.
func startDaemonWatchdog(ctx context.Context, socketPath string, interval time.Duration, send func(string) error) {
	go runDaemonWatchdog(ctx, interval, func() error { return socketAnswers(socketPath) }, send)
}

// runDaemonWatchdog is the loop without the goroutine, so a test can drive it.
// A failing probe tolerates daemonWatchdogProbeTolerance consecutive misses; after
// that the daemon stops pinging, which is what makes systemd restart it. A failing
// send is treated the same way: one EAGAIN on a full notify socket must not end the
// loop for good and let a healthy daemon be killed 90s later.
func runDaemonWatchdog(ctx context.Context, interval time.Duration, probe func() error, send func(string) error) {
	if interval <= 0 || probe == nil || send == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	probeFailures, sendFailures := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := probe(); err != nil {
				probeFailures++
				if probeFailures > daemonWatchdogProbeTolerance {
					continue // silence is the signal systemd acts on
				}
			} else {
				probeFailures = 0
			}
			if err := send("WATCHDOG=1"); err != nil {
				sendFailures++
				if sendFailures > daemonWatchdogProbeTolerance {
					return // the notify socket is gone for good; nobody is listening
				}
				continue
			}
			sendFailures = 0
		}
	}
}

package codescan

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// cbmDaemonStartTimeout bounds one `daemon start` attempt. CBM validates
// coordination markers and warms the UI asynchronously, which takes a few
// seconds on large caches.
const cbmDaemonStartTimeout = 30 * time.Second

const cbmDaemonStopTimeout = 10 * time.Second

// ErrDaemonSessionManaged reports that a CBM daemon is active but
// session-managed — the keeper retries after a short backoff.
var ErrDaemonSessionManaged = errors.New("cbm: daemon active but session-managed")

// EnsureCBMDaemon makes sure a PERMANENT CBM daemon is running. CLI calls
// then attach to it instead of spawning one internal daemon per query, and
// index runs survive client disconnects (spike 2026-09-28: 3 queries → 1
// daemon.start, 0 runtime_stopping).
//
// Success is judged ONLY by CBM's own output: `daemon start` prints either
// "daemon: started (permanent, pid N)" / "daemon: already active
// (permanent, pid N)" or "daemon: already active (session-managed, pid N)".
// The session-managed case exits 0 but leaves the main load relief
// unavailable — when no background index job is in flight, the session
// daemon is retired (`daemon stop`) and started again; the result is
// re-checked. Anything else is an error (`failed: <err>`).
func EnsureCBMDaemon(bin string) error {
	if bin == "" {
		return fmt.Errorf("cbm: codebase-memory-mcp binary not found")
	}
	out, err := cbmDaemonStart(bin)
	if err != nil {
		return fmt.Errorf("cbm: daemon start failed: %w", err)
	}
	if daemonOutputPermanent(out) {
		log.Printf("cbm: permanent daemon ensured: %s", daemonStatusLine(out))
		return nil
	}
	if daemonOutputSessionManaged(out) {
		if !indexFlightEmpty() {
			log.Printf("cbm: session-managed, retry in bg — index job in flight, not stopping daemon")
			return ErrDaemonSessionManaged
		}
		if _, err := cbmDaemonExec(bin, cbmDaemonStopTimeout, "daemon", "stop"); err != nil {
			return fmt.Errorf("cbm: daemon stop failed: %w", err)
		}
		out, err = cbmDaemonStart(bin)
		if err != nil {
			return fmt.Errorf("cbm: daemon start failed: %w", err)
		}
		if daemonOutputPermanent(out) {
			log.Printf("cbm: permanent daemon ensured: %s", daemonStatusLine(out))
			return nil
		}
		if daemonOutputSessionManaged(out) {
			return ErrDaemonSessionManaged
		}
		return fmt.Errorf("cbm: daemon start did not report a permanent daemon: %s", daemonStatusLine(out))
	}
	return fmt.Errorf("cbm: daemon start did not report a permanent daemon: %s", daemonStatusLine(out))
}

// indexFlightEmpty reports whether no background index job is running. The
// single-flight registry in bg_index.go guards daemon stop/start.
func indexFlightEmpty() bool {
	indexFlightMu.Lock()
	defer indexFlightMu.Unlock()
	return len(indexFlight) == 0
}

func cbmDaemonStart(bin string) ([]byte, error) {
	return cbmDaemonExec(bin, cbmDaemonStartTimeout, "daemon", "start")
}

func cbmDaemonExec(bin string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%v: %s", err, out)
	}
	return out, nil
}

func daemonOutputPermanent(out []byte) bool {
	// "(permanent" — the session-managed message itself contains the word
	// "permanent" ("...if you want a permanent one"), so a bare substring
	// match would misclassify it.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "(permanent") {
			return true
		}
	}
	return false
}

func daemonOutputSessionManaged(out []byte) bool {
	return strings.Contains(string(out), "session-managed")
}

func daemonStatusLine(out []byte) string {
	line := strings.TrimSpace(string(out))
	if i := strings.Index(line, "\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return line
}

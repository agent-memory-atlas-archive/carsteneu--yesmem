package setup

import (
	"fmt"
	"strings"
	"testing"
)

// YM-312: the daemon unit had Restart=always and nothing else. A process that
// lives on while its socket no longer answers (SQLite lock, wedged goroutine,
// full disk) never exits, so systemd never restarts it: the panel and every MCP
// call just time out until someone notices. Type=notify plus a watchdog makes
// systemd ask for proof of life instead of assuming it.
func TestDaemonUnitRunsAWatchdog(t *testing.T) {
	unit := buildDaemonUnit("/usr/local/bin/yesmem", ":0", "/home/u/.Xauthority", "unix:path=/run/user/1000/bus", "http://localhost:9099")

	if !strings.Contains(unit, "Type=notify") {
		t.Errorf("daemon unit is not Type=notify, so nothing waits for READY=1:\n%s", unit)
	}
	if !strings.Contains(unit, "NotifyAccess=main") {
		t.Errorf("daemon unit does not restrict the notify socket to the main process:\n%s", unit)
	}
	if !strings.Contains(unit, "WatchdogSec=") {
		t.Errorf("daemon unit has no WatchdogSec, so a wedged daemon is never restarted:\n%s", unit)
	}
	// The proxy is a plain forwarder with no socket of its own to probe; it keeps
	// the simple type until it has a liveness signal worth reporting.
	if proxy := buildProxyUnit("/usr/local/bin/yesmem"); strings.Contains(proxy, "WatchdogSec=") {
		t.Errorf("proxy unit gained a watchdog without a liveness signal:\n%s", proxy)
	}
}

// TestDaemonWatchdogOutlivesItsDeadline guards the arithmetic: systemd expects a
// ping well inside WatchdogSec, and a value that leaves no room for one late ping
// turns a slow daemon into a restart loop. Two ping intervals must fit inside the
// deadline (the daemon pings every third of it, see internal/daemon).
func TestDaemonWatchdogOutlivesItsDeadline(t *testing.T) {
	unit := buildDaemonUnit("/usr/local/bin/yesmem", ":0", "/home/u/.Xauthority", "unix:path=/run/user/1000/bus", "http://localhost:9099")
	sec := watchdogSecondsInUnit(t, unit)
	if sec < 60 {
		t.Fatalf("WatchdogSec=%ds is too tight for a daemon that bounds its own work in seconds", sec)
	}
}

// watchdogSecondsInUnit reads WatchdogSec=<n> from a unit.
func watchdogSecondsInUnit(t *testing.T, unit string) int {
	t.Helper()
	for _, line := range strings.Split(unit, "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "WatchdogSec=")
		if !found {
			continue
		}
		sec := 0
		if _, err := fmt.Sscanf(strings.TrimSuffix(strings.TrimSpace(value), "s"), "%d", &sec); err != nil {
			t.Fatalf("unparsable WatchdogSec %q: %v", value, err)
		}
		return sec
	}
	t.Fatal("no WatchdogSec line in the daemon unit")
	return 0
}

package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyDaemonUnit is a unit as installed before YM-312: simple type, no
// watchdog, still waiting for the network.
const legacyDaemonUnit = `[Unit]
Description=YesMem — Long-term memory for coding agents
After=network-online.target

[Service]
Type=simple
ExecStart=/home/u/.local/bin/yesmem daemon --replace
Restart=always
RestartSec=10
Environment="DISPLAY=:0"

[Install]
WantedBy=default.target
`

const legacyProxyUnit = `[Unit]
Description=YesMem Proxy
After=network-online.target

[Service]
Type=simple
ExecStart=/home/u/.local/bin/yesmem proxy
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`

// stubMigrationSelf pins the binary the migration vouches for. In production it is
// the running executable; a unit that starts another binary is left alone.
func stubMigrationSelf(t *testing.T, path string) {
	t.Helper()
	prev := migrationSelfPath
	migrationSelfPath = path
	t.Cleanup(func() { migrationSelfPath = prev })
}

func writeUnitFixture(t *testing.T, home, name, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// YM-312: existing installs only get the watchdog through the post-update
// migration. Without it the binary reports READY to a unit that is still
// Type=simple, so nothing ever restarts a wedged daemon on an upgraded machine.
func TestMigrateSystemdUnitsAddsTheDaemonWatchdog(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	home := t.TempDir()
	daemonPath := writeUnitFixture(t, home, "yesmem.service", legacyDaemonUnit)
	proxyPath := writeUnitFixture(t, home, "yesmem-proxy.service", legacyProxyUnit)

	changed, err := MigrateSystemdUnits(home, t.TempDir())
	if err != nil {
		t.Fatalf("MigrateSystemdUnits: %v", err)
	}
	if changed != 2 {
		t.Errorf("changed = %d, want 2 (daemon watchdog + proxy network-online)", changed)
	}

	daemon := readFile(t, daemonPath)
	for _, want := range []string{"Type=notify", "NotifyAccess=main", "WatchdogSec=90", "ExecStart=/home/u/.local/bin/yesmem daemon --replace", "Restart=always"} {
		if !strings.Contains(daemon, want) {
			t.Errorf("migrated daemon unit missing %q:\n%s", want, daemon)
		}
	}
	// Exactly once each: an earlier version inserted the block twice ([Service]
	// comes before Type=simple), and the leftover Type=simple would even have won.
	for _, directive := range []string{"Type=", "NotifyAccess=", "WatchdogSec="} {
		if got := strings.Count(daemon, directive); got != 1 {
			t.Errorf("%q appears %d times in the migrated unit, want 1:\n%s", directive, got, daemon)
		}
	}
	if strings.Contains(daemon, "Type=simple") {
		t.Errorf("migrated unit still names Type=simple, which overrides the inserted Type=notify:\n%s", daemon)
	}
	if strings.Contains(daemon, "network-online") {
		t.Errorf("migrated daemon unit still waits for the network:\n%s", daemon)
	}

	proxy := readFile(t, proxyPath)
	if strings.Contains(proxy, "WatchdogSec=") || strings.Contains(proxy, "Type=notify") {
		t.Errorf("proxy unit got a watchdog without a liveness signal:\n%s", proxy)
	}
	if strings.Contains(proxy, "network-online") {
		t.Errorf("proxy unit still waits for the network:\n%s", proxy)
	}

	// Second run must be a no-op, or every update rewrites the units.
	again, err := MigrateSystemdUnits(home, t.TempDir())
	if err != nil {
		t.Fatalf("second MigrateSystemdUnits: %v", err)
	}
	if again != 0 {
		t.Errorf("second run changed %d unit(s), want 0", again)
	}
}

// TestMigrateSystemdUnitsKeepsAnExistingWatchdog: a unit that already names a
// deadline (hand-tuned) keeps it. The migration is additive, not authoritative.
func TestMigrateSystemdUnitsKeepsAnExistingWatchdog(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	home := t.TempDir()
	path := writeUnitFixture(t, home, "yesmem.service", `[Unit]
Description=YesMem

[Service]
Type=notify
WatchdogSec=300
ExecStart=/home/u/.local/bin/yesmem daemon --replace
Restart=always

[Install]
WantedBy=default.target
`)

	if _, err := MigrateSystemdUnits(home, t.TempDir()); err != nil {
		t.Fatalf("MigrateSystemdUnits: %v", err)
	}
	unit := readFile(t, path)
	if !strings.Contains(unit, "WatchdogSec=300") {
		t.Errorf("migration overwrote a hand-tuned watchdog:\n%s", unit)
	}
	if strings.Contains(unit, "WatchdogSec=90") {
		t.Errorf("migration added a second watchdog line:\n%s", unit)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestMigrateDaemonUnitWatchdogRespectsAnExplicitType: the migration upgrades the
// default type only. A unit the operator set to exec (or anything else) keeps it —
// silently changing a supervisor decision is not a migration.
func TestMigrateDaemonUnitWatchdogRespectsAnExplicitType(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	content := "[Service]\nType=exec\nExecStart=/home/u/.local/bin/yesmem daemon\nRestart=always\n"
	if got := migrateDaemonUnitWatchdog(content); got != content {
		t.Fatalf("unit with an explicit Type=exec was rewritten:\n%s", got)
	}
}

// TestMigrateDaemonUnitWatchdogAddsATypeWhenNoneIsNamed covers the oldest units:
// no Type= line at all, so the directives go behind [Service].
func TestMigrateDaemonUnitWatchdogAddsATypeWhenNoneIsNamed(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	content := "[Service]\nExecStart=/home/u/.local/bin/yesmem daemon\nRestart=always\n"
	got := migrateDaemonUnitWatchdog(content)
	for _, want := range []string{"Type=notify", "NotifyAccess=main", "WatchdogSec=90"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("%q appears %d times, want 1:\n%s", want, strings.Count(got, want), got)
		}
	}
	if !strings.Contains(got, "ExecStart=/home/u/.local/bin/yesmem daemon") {
		t.Errorf("migration dropped the ExecStart line:\n%s", got)
	}
}

// TestMigrateDaemonUnitWatchdogSkipsAForeignBinary is the capability guard: a unit
// that starts another yesmem binary must not be switched to Type=notify, because
// that binary may predate the READY report and would then never start.
func TestMigrateDaemonUnitWatchdogSkipsAForeignBinary(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	content := "[Service]\nType=simple\nExecStart=/opt/other/yesmem daemon\nRestart=always\n"
	if got := migrateDaemonUnitWatchdog(content); got != content {
		t.Fatalf("unit of a foreign binary was rewritten:\n%s", got)
	}
}

// TestMigrateDaemonUnitWatchdogAddsTheStartDeadlineToAHandTunedUnit: a unit that
// already names WatchdogSec keeps the value but still needs the start deadline, or
// a cold start looks like a hang.
func TestMigrateDaemonUnitWatchdogAddsTheStartDeadlineToAHandTunedUnit(t *testing.T) {
	stubMigrationSelf(t, "/home/u/.local/bin/yesmem")
	content := "[Service]\nType=notify\nWatchdogSec=300\nExecStart=/home/u/.local/bin/yesmem daemon\nRestart=always\n"
	got := migrateDaemonUnitWatchdog(content)
	if strings.Count(got, "WatchdogSec=") != 1 || !strings.Contains(got, "WatchdogSec=300") {
		t.Fatalf("hand-tuned deadline was changed:\n%s", got)
	}
	if strings.Count(got, "TimeoutStartSec=") != 1 {
		t.Fatalf("TimeoutStartSec appears %d times, want 1:\n%s", strings.Count(got, "TimeoutStartSec="), got)
	}
	if again := migrateDaemonUnitWatchdog(got); again != got {
		t.Fatalf("second run changed the unit again:\n%s", again)
	}
}

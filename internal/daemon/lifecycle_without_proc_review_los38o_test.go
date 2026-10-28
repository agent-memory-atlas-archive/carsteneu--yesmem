package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Los 38o review follow-ups: the branches the first round left uncovered.

// TestStopRoleFallsBackToTheListingWhenLaunchdRefuses: an agent that exists on
// disk but is not loaded makes bootout fail. The listing is the second source and
// must still run — otherwise the stop does nothing in exactly the case the fallback
// exists for.
func TestStopRoleFallsBackToTheListingWhenLaunchdRefuses(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	prevExe := selfBinaryPath
	selfBinaryPath = "/opt/yesmem/yesmem"
	t.Cleanup(func() { selfBinaryPath = prevExe })
	stubServicePlatform(t, "darwin")
	writeDaemonPlist(t, home, "daemon", "/opt/yesmem/yesmem")
	calls := stubLaunchctl(t, map[string]error{
		"bootout": fmt.Errorf("Could not find service"),
		"unload":  fmt.Errorf("Could not find specified service"),
	})
	dataDir := filepath.Join(home, ".claude", "yesmem")
	stubProcessListing(t, []string{"  4321 /opt/yesmem/yesmem daemon"})
	signals := stubRoleStopSeams(t, func(int) bool { return true }, nil)

	pids, stopped, err := StopRole("daemon", dataDir)
	if err != nil {
		t.Fatalf("StopRole: %v (the listing should have taken over)", err)
	}
	if stopped {
		t.Error("stopped = true although launchd refused")
	}
	if len(pids) != 1 || pids[0] != 4321 {
		t.Fatalf("pids = %v, want [4321] from the listing", pids)
	}
	if got := strings.Join(*signals, ","); got != "TERM 4321" {
		t.Errorf("signals = %q, want TERM 4321", got)
	}
	if len(*calls) != 2 {
		t.Errorf("launchctl calls = %v, want bootout then unload", *calls)
	}
}

// TestStopRoleReportsTheLaunchdRefusalWhenNothingElseMatched: the error must not be
// swallowed when the listing found nothing either.
func TestStopRoleReportsTheLaunchdRefusalWhenNothingElseMatched(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	prevExe := selfBinaryPath
	selfBinaryPath = "/opt/yesmem/yesmem"
	t.Cleanup(func() { selfBinaryPath = prevExe })
	stubServicePlatform(t, "darwin")
	writeDaemonPlist(t, home, "daemon", "/opt/yesmem/yesmem")
	stubLaunchctl(t, map[string]error{"bootout": fmt.Errorf("Could not find service"), "unload": fmt.Errorf("nope")})
	stubProcessListing(t, nil)
	stubRoleStopSeams(t, func(int) bool { return true }, nil)

	if _, _, err := StopRole("daemon", filepath.Join(home, ".claude", "yesmem")); err == nil {
		t.Fatal("StopRole reported success although launchd refused and the listing matched nothing")
	}
}

// TestStopRolePrefersThePidFileIdentity: the data dir's PID file names the instance
// that belongs to this installation, which is narrower than any path match. A second
// instance of the same binary must survive.
func TestStopRolePrefersThePidFileIdentity(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	prevExe := selfBinaryPath
	selfBinaryPath = "/opt/yesmem/yesmem"
	t.Cleanup(func() { selfBinaryPath = prevExe })
	stubServicePlatform(t, "darwin")
	stubLaunchctl(t, nil)

	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "daemon.pid"), []byte("4100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubProcessListing(t, []string{
		"  4100 /opt/yesmem/yesmem daemon", // the pid file's own
		"  4200 /opt/yesmem/yesmem daemon", // another HOME, same binary
	})
	signals := stubRoleStopSeams(t, func(int) bool { return true }, nil)

	pids, _, err := StopRole("daemon", dataDir)
	if err != nil {
		t.Fatalf("StopRole: %v", err)
	}
	if len(pids) != 1 || pids[0] != 4100 {
		t.Fatalf("pids = %v, want only the pid the data dir's PID file names", pids)
	}
	if got := strings.Join(*signals, ","); got != "TERM 4100" {
		t.Errorf("signals = %q, want TERM 4100", got)
	}
}

// TestStopRoleRejectsAnUnknownRole: the role reaches a plist path and a launchd
// target. Both call sites pass literals, but the boundary has to say no to anything
// else rather than build a path outside ~/Library/LaunchAgents.
func TestStopRoleRejectsAnUnknownRole(t *testing.T) {
	stubServicePlatform(t, "darwin")
	calls := stubLaunchctl(t, nil)
	if _, _, err := StopRole("../../etc", t.TempDir()); err == nil {
		t.Fatal("StopRole accepted a role that is not daemon or proxy")
	}
	if _, err := StartRole("../daemon"); err == nil {
		t.Fatal("StartRole accepted a role that is not daemon or proxy")
	}
	if len(*calls) != 0 {
		t.Errorf("launchctl was called with a rejected role: %v", *calls)
	}
}

// TestPlistProgramFollowsLaunchdsPrecedence: when a plist names both Program and
// ProgramArguments, launchd starts Program. The identity check has to read the same
// one.
func TestPlistProgramFollowsLaunchdsPrecedence(t *testing.T) {
	content := `<plist><dict>
	<key>ProgramArguments</key>
	<array>
		<string>/opt/yesmem/yesmem</string>
		<string>daemon</string>
	</array>
	<key>Program</key>
	<string>/tmp/other</string>
</dict></plist>`
	got, err := plistProgram(content)
	if err != nil {
		t.Fatalf("plistProgram: %v", err)
	}
	if got != "/tmp/other" {
		t.Fatalf("plistProgram = %q, want /tmp/other (Program wins over ProgramArguments)", got)
	}
}

// TestStartRoleRefusesAPlistOfAnotherBinary: the start path has the same identity
// rule as the stop path.
func TestStartRoleRefusesAPlistOfAnotherBinary(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	prevExe := selfBinaryPath
	selfBinaryPath = "/opt/yesmem/yesmem"
	t.Cleanup(func() { selfBinaryPath = prevExe })
	stubServicePlatform(t, "darwin")
	writeDaemonPlist(t, home, "daemon", "/usr/local/bin/yesmem")
	calls := stubLaunchctl(t, nil)

	if _, err := StartRole("daemon"); err == nil {
		t.Fatal("StartRole bootstrapped an agent that starts another binary")
	}
	if len(*calls) != 0 {
		t.Errorf("launchctl was called for a foreign agent: %v", *calls)
	}
}

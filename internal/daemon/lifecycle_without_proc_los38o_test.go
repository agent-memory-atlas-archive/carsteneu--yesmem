package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Los 38o / YM-333: without /proc `yesmem stop` signalled nothing and `yesmem
// restart` aborted, because the identity check had exactly one source (the process
// table). These tests drive the two sources that exist on such a platform (launchd,
// then the process listing) through seams, so they run on Linux too.

// stubServicePlatform pins the platform whose service path the code under test
// takes: "darwin" reaches the launchd branch, "linux" keeps the process table.
func stubServicePlatform(t *testing.T, platform string) {
	t.Helper()
	prev := servicePlatform
	servicePlatform = platform
	t.Cleanup(func() { servicePlatform = prev })
}

// stubLaunchctl records the launchctl invocations and answers with the error of
// the map entry whose verb matches ("" for none).
func stubLaunchctl(t *testing.T, failVerbs map[string]error) *[]string {
	t.Helper()
	var calls []string
	prev := launchctlRun
	launchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if len(args) > 0 {
			if err, ok := failVerbs[args[0]]; ok {
				return err
			}
		}
		return nil
	}
	t.Cleanup(func() { launchctlRun = prev })
	return &calls
}

// stubProcessListing replaces the ps call with a fixture listing.
func stubProcessListing(t *testing.T, lines []string) {
	t.Helper()
	prev := listProcesses
	listProcesses = func() (string, error) { return strings.Join(lines, "\n") + "\n", nil }
	t.Cleanup(func() { listProcesses = prev })
}

func writeDaemonPlist(t *testing.T, home, role, program string) string {
	t.Helper()
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(dir, "com.yesmem."+role+".plist")
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.yesmem.%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>%s</string>
	</array>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`, role, program, role)
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return plist
}

// TestStopRoleStopsTheLaunchdServiceThatOwnsThisInstallation: the plist is the
// identity on this platform. KeepAlive=true means a SIGTERM is undone at once, so
// the service has to leave the launchd domain.
func TestStopRoleStopsTheLaunchdServiceThatOwnsThisInstallation(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	prevExe := selfBinaryPath
	selfBinaryPath = "/opt/yesmem/yesmem"
	t.Cleanup(func() { selfBinaryPath = prevExe })
	stubServicePlatform(t, "darwin")
	writeDaemonPlist(t, home, "daemon", "/opt/yesmem/yesmem")
	calls := stubLaunchctl(t, nil)

	pids, stopped, err := StopRole("daemon", filepath.Join(home, ".claude", "yesmem"))
	if err != nil {
		t.Fatalf("StopRole: %v", err)
	}
	if len(pids) != 0 {
		t.Errorf("pids = %v, want none: launchd owns the process", pids)
	}
	if !stopped {
		t.Errorf("stopped = false, want true: the launchd agent was booted out")
	}
	want := fmt.Sprintf("bootout gui/%d/com.yesmem.daemon", os.Getuid())
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("launchctl calls = %v, want exactly [%s]", *calls, want)
	}
}

// TestStopRoleRefusesAPlistThatStartsAnotherBinary is the YM-297 guard on this
// platform: a value another installation wrote is not this installation's service.
func TestStopRoleRefusesAPlistThatStartsAnotherBinary(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	prevExe := selfBinaryPath
	selfBinaryPath = "/opt/yesmem/yesmem"
	t.Cleanup(func() { selfBinaryPath = prevExe })
	stubServicePlatform(t, "darwin")
	writeDaemonPlist(t, home, "daemon", "/usr/local/bin/yesmem")
	calls := stubLaunchctl(t, nil)
	stubProcessListing(t, []string{"  4711 /usr/local/bin/yesmem daemon"})

	if _, _, err := StopRole("daemon", filepath.Join(home, ".claude", "yesmem")); err == nil {
		t.Fatal("StopRole stopped a service whose plist starts another binary")
	}
	if len(*calls) != 0 {
		t.Errorf("launchctl was called for a foreign service: %v", *calls)
	}
}

// TestStopRoleFindsTheDaemonInTheProcessListingWithoutAPlist: an install without
// launchd agents (started by hand, or set up before the plists existed) still has
// to be stoppable. The listing supplies the argv; the identity is this binary's
// absolute path plus the role, which is exactly what the PID-file path checks.
func TestStopRoleFindsTheDaemonInTheProcessListingWithoutAPlist(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	prevExe := selfBinaryPath
	selfBinaryPath = "/opt/yesmem/yesmem"
	t.Cleanup(func() { selfBinaryPath = prevExe })
	stubServicePlatform(t, "darwin")
	calls := stubLaunchctl(t, nil)
	stubProcessListing(t, []string{
		"  4100 /opt/yesmem/yesmem daemon --replace", // ours
		"  4101 /opt/yesmem/yesmem proxy",            // the other role
		"  4102 /bin/bash -c grep yesmem daemon",     // only mentions the words
		"  4103 /opt/yesmem/yesmem.test daemon",      // a worktree test binary
		"  4104 /usr/local/bin/yesmem daemon",        // another installation
	})
	signals := stubRoleStopSeams(t, func(pid int) bool { return pid == 4100 }, nil)

	pids, stopped, err := StopRole("daemon", filepath.Join(home, ".claude", "yesmem"))
	if err != nil {
		t.Fatalf("StopRole: %v", err)
	}
	if stopped {
		t.Errorf("stopped = true without a launchd agent; the listing path signals pids instead")
	}
	if len(pids) != 1 || pids[0] != 4100 {
		t.Fatalf("pids = %v, want [4100]", pids)
	}
	if got := strings.Join(*signals, ","); got != "TERM 4100" {
		t.Errorf("signals = %q, want %q", got, "TERM 4100")
	}
	if len(*calls) != 0 {
		t.Errorf("launchctl was called although no plist exists: %v", *calls)
	}
}

// TestStopRoleKeepsTheProcessTablePathOnLinux: the launchd branch must stay off
// where /proc exists; the verified data-dir identity is stronger than anything the
// listing can offer.
func TestStopRoleKeepsTheProcessTablePathOnLinux(t *testing.T) {
	stubServicePlatform(t, "linux")
	calls := stubLaunchctl(t, nil)
	listingUsed := false
	prevListing := listProcesses
	listProcesses = func() (string, error) { listingUsed = true; return "", nil }
	t.Cleanup(func() { listProcesses = prevListing })

	root := t.TempDir()
	useProcRoot(t, root)
	writeProcFixture(t, root, 20, []string{"/opt/yesmem/yesmem", "daemon"}, []string{"HOME=" + t.TempDir()})
	signals := stubRoleStopSeams(t, func(int) bool { return true }, nil)

	StopRole("daemon", filepath.Join(t.TempDir(), ".claude", "yesmem"))
	if listingUsed {
		t.Error("the process listing was used on a platform with a process table")
	}
	if len(*calls) != 0 {
		t.Errorf("launchctl was called on linux: %v", *calls)
	}
	_ = signals
}

// TestStartRoleBootstrapsTheServiceAgain: after a stop the service has left the
// domain, so starting it needs bootstrap (unload/load on older systems), never a
// child process spawned by the CLI.
func TestStartRoleBootstrapsTheServiceAgain(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	prevExe := selfBinaryPath
	selfBinaryPath = "/opt/yesmem/yesmem"
	t.Cleanup(func() { selfBinaryPath = prevExe })
	stubServicePlatform(t, "darwin")
	plist := writeDaemonPlist(t, home, "daemon", "/opt/yesmem/yesmem")
	calls := stubLaunchctl(t, nil)

	started, err := StartRole("daemon")
	if err != nil {
		t.Fatalf("StartRole: %v", err)
	}
	if !started {
		t.Errorf("started = false, want true: the agent was bootstrapped")
	}
	want := fmt.Sprintf("bootstrap gui/%d %s", os.Getuid(), plist)
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("launchctl calls = %v, want exactly [%s]", *calls, want)
	}
}

// TestStartRoleFallsBackToLoad: bootstrap only exists in current launchd. An older
// system answers with an error, and the install still has to come back up.
func TestStartRoleFallsBackToLoad(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	prevExe := selfBinaryPath
	selfBinaryPath = "/opt/yesmem/yesmem"
	t.Cleanup(func() { selfBinaryPath = prevExe })
	stubServicePlatform(t, "darwin")
	plist := writeDaemonPlist(t, home, "daemon", "/opt/yesmem/yesmem")
	calls := stubLaunchctl(t, map[string]error{"bootstrap": fmt.Errorf("unknown subcommand")})

	if _, err := StartRole("daemon"); err != nil {
		t.Fatalf("StartRole: %v", err)
	}
	want := "load " + plist
	if got := (*calls)[len(*calls)-1]; got != want {
		t.Errorf("last launchctl call = %q, want %q", got, want)
	}
}

// TestStartRoleDoesNothingWithoutAPlist: without launchd agents the caller spawns
// the daemon itself, exactly as before.
func TestStartRoleDoesNothingWithoutAPlist(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)
	stubServicePlatform(t, "darwin")
	calls := stubLaunchctl(t, nil)

	started, err := StartRole("daemon")
	if err != nil {
		t.Fatalf("StartRole: %v", err)
	}
	if started {
		t.Errorf("started = true without a launchd agent; the caller spawns the process")
	}
	if len(*calls) != 0 {
		t.Errorf("launchctl was called without a plist: %v", *calls)
	}
}

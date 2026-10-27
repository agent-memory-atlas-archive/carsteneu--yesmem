package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// servicePlatform is the OS whose service manager owns this installation's
// processes. Seamed so a test can drive the launchd path on a host that has a
// process table.
var servicePlatform = runtime.GOOS

// launchdTimeout bounds one launchctl call. It is local IPC, but a wedged launchd
// must not hold a stop or a restart forever.
const launchdTimeout = 15 * time.Second

// launchctlRun runs one launchctl command. Seamed for tests: there is no launchd on
// the host the suite runs on.
var launchctlRun = func(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), launchdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "launchctl", args...).Run()
}

// listProcesses returns the ps listing, one "pid command" line per process.
// Seamed for tests, for the same reason.
var listProcesses = func() (string, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "args=").Output()
	return string(out), err
}

// StopRole ends this installation's instance of role and reports how it did it:
// the process ids it signalled directly, or stopped=true when the platform's
// service manager ended the process itself.
//
// On a platform with a process table the verified identity of StopOwnedRole is
// used unchanged. Where that table does not exist (macOS), the check had exactly
// one source and therefore signalled nothing at all, so `yesmem stop` did nothing
// and `yesmem restart` aborted (YM-333). The two sources that do exist there are
// launchd, which states the identity itself through the agent it keeps loaded, and
// the process listing, whose identity is the data dir's PID file or, failing that,
// this binary's path plus the role argument.
func StopRole(role, dataDir string) (pids []int, stopped bool, err error) {
	if err := validRole(role); err != nil {
		return nil, false, err
	}
	if servicePlatform != "darwin" {
		return StopOwnedRole(role, dataDir), false, nil
	}
	stoppedByService, serviceErr := stopLaunchdRole(role)
	if stoppedByService {
		return nil, true, nil
	}
	// A launchd refusal must not swallow the second source: an agent that is not
	// loaded (started by hand, booted out earlier, or a wedged launchd) is exactly
	// the case the listing exists for. The refusal is reported only when the
	// listing found nothing either.
	pids = stopRoleFromListing(role, dataDir)
	if len(pids) > 0 || serviceErr == nil {
		return pids, false, nil
	}
	return nil, false, serviceErr
}

// StartRole starts the role again after StopRole ended it, through the supervisor
// that owns it. started=false means there is no such supervisor here and the caller
// spawns the process exactly as before.
func StartRole(role string) (started bool, err error) {
	if err := validRole(role); err != nil {
		return false, err
	}
	if servicePlatform != "darwin" {
		return false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	path, program, present, err := readLaunchdPlist(home, role)
	if err != nil || !present {
		return false, err
	}
	if err := plistStartsThisBinary(path, program); err != nil {
		return false, err
	}
	// An agent that was booted out is not in the domain any more, so bootstrap is
	// what brings it back. Older launchd knows only load.
	if berr := launchctlRun("bootstrap", launchdDomain(), path); berr != nil {
		if lerr := launchctlRun("load", path); lerr != nil {
			return false, fmt.Errorf("start %s service: launchctl bootstrap: %v; launchctl load: %v", role, berr, lerr)
		}
	}
	return true, nil
}

// validRole guards the two places that build a plist path and a launchd target from
// the role. Both call sites pass literals, but a role carrying a path separator
// would leave ~/Library/LaunchAgents, so the check belongs at the boundary.
func validRole(role string) error {
	if role != "daemon" && role != "proxy" {
		return fmt.Errorf("unknown role %q (want daemon or proxy)", role)
	}
	return nil
}

// stopLaunchdRole ends the role's launchd agent. stopped=false with err=nil means
// this installation has no agent for that role.
func stopLaunchdRole(role string) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	path, program, present, err := readLaunchdPlist(home, role)
	if err != nil || !present {
		return false, err
	}
	if err := plistStartsThisBinary(path, program); err != nil {
		return false, err
	}
	// The plists carry KeepAlive=true: a signalled process would be restarted by
	// launchd at once, so the agent has to leave the domain.
	if berr := launchctlRun("bootout", launchdTarget(role)); berr != nil {
		if lerr := launchctlRun("unload", path); lerr != nil {
			return false, fmt.Errorf("stop %s service: launchctl bootout: %v; launchctl unload: %v", role, berr, lerr)
		}
	}
	return true, nil
}

// stopRoleFromListing ends the role instances the process listing shows as this
// installation's and returns their pids. The order is the process-table path's
// (SIGTERM, grace, re-check, SIGKILL); what it cannot do is date a process, because
// the listing carries no start time here — the re-check compares the argv instead,
// and a pid that has to be reused by a process with this very binary and this very
// role argument before the signal is effectively the same installation.
func stopRoleFromListing(role, dataDir string) []int {
	out, err := listProcesses()
	if err != nil {
		log.Printf("cannot read the process listing, so no %s process can be verified as this installation's — stopping nothing: %v", role, err)
		return nil
	}
	if selfBinaryPath == "" {
		log.Printf("own binary path unknown, so no %s process can be verified as this installation's — stopping nothing", role)
		return nil
	}
	var signalled []int
	for _, pid := range listingTargets(role, dataDir, parseListing(out)) {
		// The listing is a snapshot; re-read before the first, reversible signal.
		if !listingStillNamesOurRole(pid, role) {
			continue
		}
		if err := daemonProcSignal(pid, syscall.SIGTERM); err != nil {
			continue
		}
		signalled = append(signalled, pid)
	}
	for _, pid := range signalled {
		if waitDaemonGone(pid, daemonTermGrace) {
			continue
		}
		// Re-check before the irreversible signal: the number may have been
		// recycled onto another process during the grace.
		if !listingStillNamesOurRole(pid, role) {
			log.Printf("PID %d did not exit within %s and no longer identifies as this installation's %s — not sending SIGKILL", pid, daemonTermGrace, role)
			continue
		}
		_ = daemonProcSignal(pid, syscall.SIGKILL)
		if !waitDaemonGone(pid, daemonTermGrace) {
			log.Printf("PID %d survived SIGKILL", pid)
		}
	}
	return signalled
}

// listingTargets picks the pids to stop. The data dir's own PID file, when the
// listing confirms it, is the narrow identity: it proves which installation the
// process belongs to without an environment the listing cannot show. Only without
// such a pid do the listing's own facts (binary path plus role) decide, and then
// several instances of that binary may match.
func listingTargets(role, dataDir string, listed map[int][]string) []int {
	if role == "daemon" {
		if pid, ok := pidFileOwner(dataDir); ok {
			if argv, found := listed[pid]; found && listingNamesOurRole(argv, role) {
				return []int{pid}
			}
		}
	}
	var targets []int
	for pid, argv := range listed {
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		if listingNamesOurRole(argv, role) {
			targets = append(targets, pid)
		}
	}
	sort.Ints(targets)
	return targets
}

// parseListing indexes the ps output by pid.
func parseListing(out string) map[int][]string {
	entries := make(map[int][]string)
	for _, line := range strings.Split(out, "\n") {
		if pid, argv, ok := parseProcessLine(line); ok {
			entries[pid] = argv
		}
	}
	return entries
}

// listingStillNamesOurRole re-reads the listing for one pid.
func listingStillNamesOurRole(pid int, role string) bool {
	out, err := listProcesses()
	if err != nil {
		return false
	}
	argv, found := parseListing(out)[pid]
	return found && listingNamesOurRole(argv, role)
}

// parseProcessLine splits one ps line into pid and argv. A truncated or malformed
// line is not a process.
func parseProcessLine(line string) (int, []string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, nil, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, nil, false
	}
	return pid, fields[1:], true
}

// listingNamesOurRole is the identity a process listing can prove: this binary as
// argv[0], the role as argv[1]. The PID-file path of the process-table variant
// checks the same argv against the kernel's record of the executable; here the
// listing shows the path as it was invoked, so both the literal and the symlink
// resolved form count. A shell that merely mentions the words cannot match (its
// argv[0] is the interpreter), and a worktree test binary fails isYesmemBinary.
//
// A binary path containing whitespace cannot be read back from an unfolded ps line
// and therefore does not match; such an install has to use the launchd agent. And
// unlike the process-table variant the listing carries no environment, so two
// installations that share the binary path are only told apart by their PID files.
func listingNamesOurRole(argv []string, role string) bool {
	if len(argv) < 2 || selfBinaryPath == "" {
		return false
	}
	if !sameBinaryPath(argv[0]) {
		return false
	}
	if !isYesmemBinary(filepath.Base(argv[0])) {
		return false
	}
	return argv[1] == role
}

// sameBinaryPath compares a listing's argv[0] with this process's own binary. This
// process resolved its path through symlinks, the listing shows the invocation, so
// both forms are compared.
func sameBinaryPath(exe string) bool {
	clean := filepath.Clean(exe)
	if clean == filepath.Clean(selfBinaryPath) {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved) == filepath.Clean(selfBinaryPath)
	}
	return false
}

// launchdDomain is the per-user launchd domain the agents live in.
func launchdDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

// launchdLabel is the agent label the installer writes into the plist.
func launchdLabel(role string) string { return "com.yesmem." + role }

// launchdTarget addresses the loaded agent of a role.
func launchdTarget(role string) string { return launchdDomain() + "/" + launchdLabel(role) }

// launchdPlistPath is where the installer puts the agent for a role.
func launchdPlistPath(home, role string) string {
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel(role)+".plist")
}

// readLaunchdPlist returns the agent file of a role, the program it starts, and
// whether such a file exists.
func readLaunchdPlist(home, role string) (path, program string, present bool, err error) {
	path = launchdPlistPath(home, role)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, "", false, nil
		}
		return path, "", false, err
	}
	program, perr := plistProgram(string(data))
	if perr != nil {
		return path, "", true, fmt.Errorf("%s: %w", path, perr)
	}
	return path, program, true, nil
}

// plistStartsThisBinary is the identity the plist itself states: never touch an
// agent that starts a binary other than the one asking.
func plistStartsThisBinary(path, program string) error {
	if selfBinaryPath == "" {
		return fmt.Errorf("own binary path unknown, so %s cannot be verified as this installation's agent", path)
	}
	if filepath.Clean(program) != filepath.Clean(selfBinaryPath) {
		return fmt.Errorf("%s starts %s, not this binary (%s) — not touching that agent; remove the file if it is stale", path, program, selfBinaryPath)
	}
	return nil
}

// plistProgram extracts the executable a launchd plist starts. Program wins over
// ProgramArguments, which is launchd's own precedence: a plist that names both
// starts Program, and the identity check has to look at what would really run.
func plistProgram(content string) (string, error) {
	lines := strings.Split(content, "\n")
	for _, key := range []string{"<key>Program</key>", "<key>ProgramArguments</key>"} {
		for i, line := range lines {
			if strings.TrimSpace(line) != key {
				continue
			}
			for _, next := range lines[i+1:] {
				if value, ok := plistStringValue(next); ok {
					return value, nil
				}
			}
		}
	}
	return "", errors.New("plist names no program to start")
}

// plistStringValue reads a one-line <string>value</string> element.
func plistStringValue(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "<string>") || !strings.HasSuffix(trimmed, "</string>") {
		return "", false
	}
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "<string>"), "</string>"))
	if value == "" {
		return "", false
	}
	return value, true
}

package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxProcEnvironBytes bounds the environ read. The environment of a process is
// caller-controlled and can be megabytes; only the first entries decide which
// data dir it runs with.
const maxProcEnvironBytes = 64 << 10

// processEnviron reads a process's environment. It is the only way to learn which
// data dir an already running instance resolved: the flag set carries no data dir,
// so the process took it from YESMEM_DATA_DIR or from its own HOME.
func processEnviron(pid int) []string {
	f, err := os.Open(fmt.Sprintf("%s/%d/environ", procRoot, pid))
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxProcEnvironBytes))
	if err != nil {
		return nil
	}
	if len(data) == 0 {
		// A process can clear its environment. An empty environ is not evidence
		// for any data dir, and the caller treats nil as unverifiable.
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
}

// effectiveDataDir resolves the data dir a process with this environment runs
// with. Only HOME counts: no yesmem service binary reads YESMEM_DATA_DIR — the
// daemon and the proxy both derive their state dir from the user's home, and the
// variable is inert for them (the MCP client sets it for its own children, who run
// as `yesmem mcp` and never match a service role). A stray value in the environment
// must therefore not reclassify a process as another installation.
func effectiveDataDir(environ []string, fallbackHome string) string {
	var home string
	for _, kv := range environ {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key != "HOME" {
			continue
		}
		home = value
	}
	if home == "" {
		home = fallbackHome
	}
	return filepath.Join(home, ".claude", "yesmem")
}

// isYesmemBinary reports whether a basename names a yesmem binary. A Go test
// binary from a yesmem worktree (yesmem.test, the name `go test -c` produces) is
// not one: its argv carries -test.* flags and it never runs the daemon loop, but a
// fixture or a stray argument could still put "daemon" in argv[1].
func isYesmemBinary(base string) bool {
	if strings.HasSuffix(base, ".test") {
		return false
	}
	return strings.Contains(base, "yesmem")
}

// ownRoleProcess reports whether pid is an instance of role (daemon or proxy)
// belonging to dataDir. Identity has two parts: the argv structure names a yesmem
// subcommand (a pgrep name match does not — a shell carrying the words in its
// command line matches that), and the process's own environment resolves to the
// same data dir.
func ownRoleProcess(pid int, role, dataDir, fallbackHome string) bool {
	if pid <= 1 || pid == os.Getpid() {
		return false
	}
	argv := strings.Split(procCmdline(pid), "\x00")
	if len(argv) < 2 || !isYesmemBinary(filepath.Base(argv[0])) || argv[1] != role {
		return false
	}
	environ := processEnviron(pid)
	if environ == nil {
		// No readable environment: the data dir cannot be established, and an
		// unverifiable process is not ours to stop.
		return false
	}
	return effectiveDataDir(environ, fallbackHome) == filepath.Clean(dataDir)
}

// roleInstance is one process the scan selected: the pid plus its start time.
// The pair is what makes it that process — a bare pid can be reused by an
// unrelated process between the scan and the signal.
type roleInstance struct {
	pid   int
	start time.Time
	known bool // false when the start time could not be read
}

// ownedRoleInstances lists this installation's role instances. The daemon's PID
// file is consulted too, so a PID file that names something else is noticed
// instead of silently kept.
func ownedRoleInstances(role, dataDir string, selfPID int) []roleInstance {
	home, _ := os.UserHomeDir()
	var instances []roleInstance
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		// Without the process table there is nothing to verify against, and the
		// name alone is not evidence (that sweep is what this check replaces).
		// Say so instead of stopping nothing silently.
		log.Printf("cannot read %s, so no %s process can be verified as this installation's — stopping nothing; end it by hand if it must go", procRoot, role)
		return nil
	}
	for _, entry := range entries {
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || pid == selfPID {
			continue
		}
		if ownRoleProcess(pid, role, dataDir, home) {
			start, known := procStartTime(pid)
			instances = append(instances, roleInstance{pid: pid, start: start, known: known})
		}
	}
	if role == "daemon" {
		if pid, ok := pidFileOwner(dataDir); ok && pid != selfPID {
			known := false
			for _, have := range instances {
				if have.pid == pid {
					known = true
					break
				}
			}
			if !known && !daemonProcGone(pid) {
				log.Printf("PID %d in %s is alive but is not this installation's daemon (argv or data dir differ) — not signalling it", pid, pidFilePath(dataDir))
			}
		}
	}
	return instances
}

// ownedRolePIDs is the pid view of ownedRoleInstances.
func ownedRolePIDs(role, dataDir string, selfPID int) []int {
	instances := ownedRoleInstances(role, dataDir, selfPID)
	pids := make([]int, 0, len(instances))
	for _, inst := range instances {
		pids = append(pids, inst.pid)
	}
	return pids
}

// sameInstance re-reads the process identity right before a signal: argv, data
// dir, and the start time that distinguishes this process from whatever inherits
// its pid afterwards.
func sameInstance(inst roleInstance, role, dataDir, fallbackHome string) bool {
	if !ownRoleProcess(inst.pid, role, dataDir, fallbackHome) {
		return false
	}
	if !inst.known {
		return true // the scan could not date it; argv and data dir still matched
	}
	start, ok := procStartTime(inst.pid)
	return ok && start.Equal(inst.start)
}

// pidFileOwner reads the PID a data dir's PID file names.
func pidFileOwner(dataDir string) (int, bool) {
	raw, err := os.ReadFile(pidFilePath(dataDir))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// StopOwnedRole stops every live instance of role that belongs to dataDir and
// returns the pids it signalled.
//
// Why so narrow: the former pgrep sweep signalled every process whose command line
// contained "yesmem daemon" — the systemd instance, another data dir's daemon, a
// worktree's *.test binary, a shell that merely mentioned the words. On
// 2026-10-17 that class of sweep took the live daemon down and with it every
// running agent (YM-297).
func StopOwnedRole(role, dataDir string) []int {
	instances := ownedRoleInstances(role, dataDir, os.Getpid())
	home, _ := os.UserHomeDir()
	var signalled []roleInstance
	for _, inst := range instances {
		// Check-to-use window: the scan finished some milliseconds ago and the
		// number may already belong to another process. Verify before the first
		// signal, exactly as before the last one.
		if !sameInstance(inst, role, dataDir, home) {
			continue
		}
		if err := daemonProcSignal(inst.pid, syscall.SIGTERM); err != nil {
			continue
		}
		signalled = append(signalled, inst)
	}
	for _, inst := range signalled {
		if waitDaemonGone(inst.pid, daemonTermGrace) {
			continue
		}
		// Re-verify before the irreversible signal: during the grace the number
		// may have been recycled onto an unrelated process.
		if !sameInstance(inst, role, dataDir, home) {
			log.Printf("PID %d did not exit within %s and no longer identifies as this installation's %s — not sending SIGKILL", inst.pid, daemonTermGrace, role)
			continue
		}
		_ = daemonProcSignal(inst.pid, syscall.SIGKILL)
		if !waitDaemonGone(inst.pid, daemonTermGrace) {
			log.Printf("PID %d survived SIGKILL", inst.pid)
		}
	}
	pids := make([]int, 0, len(signalled))
	for _, inst := range signalled {
		pids = append(pids, inst.pid)
	}
	return pids
}

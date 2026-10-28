package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ensureSingleInstance checks if another daemon is running and handles it.
func ensureSingleInstance(dataDir string, replace bool) error {
	sockPath := SocketPath(dataDir)
	pidPath := pidFilePath(dataDir)

	// Step 1: Try socket probe
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err == nil {
		// Socket reachable — daemon is running
		defer conn.Close()

		// Send ping to verify
		encoder := json.NewEncoder(conn)
		decoder := json.NewDecoder(conn)
		encoder.Encode(Request{Method: "ping"})
		var resp Response
		decoder.Decode(&resp)

		if !replace {
			pid := readPidFile(pidPath)
			return fmt.Errorf("daemon already running (PID %s, socket %s). Use --replace to restart", pid, sockPath)
		}

		// --replace: stop the instance this data dir owns
		log.Println("Replacing existing daemon...")
		stopped, err := stopDaemonFromPidFile(dataDir)
		if err != nil {
			return fmt.Errorf("cannot replace daemon: %w", err)
		}
		if !stopped {
			// A daemon answers here, but nothing in the PID file identifies it.
			// Removing its socket would strand it while a second instance took
			// over the data dir.
			return fmt.Errorf("cannot replace daemon: a daemon answers on %s but %s names no process that could be verified as this installation's daemon; remove that file if it is stale", sockPath, pidFilePath(dataDir))
		}
		os.Remove(sockPath)
		os.Remove(pidPath)
		time.Sleep(2 * time.Second)
	} else {
		// Socket not reachable — clean up stale files
		if _, err := os.Stat(sockPath); err == nil {
			log.Println("Removing stale socket file")
			os.Remove(sockPath)
		}
		if _, err := os.Stat(pidPath); err == nil {
			pid := readPidFile(pidPath)
			if pid != "" && isProcessZombie(pid) {
				// Zombie: appears alive to signal 0 but is effectively dead
				log.Printf("Removing zombie process PID file (PID %s)", pid)
				os.Remove(pidPath)
			} else if pid != "" && !isProcessAlive(pid) {
				log.Println("Removing stale PID file")
				os.Remove(pidPath)
			} else if pid != "" && isProcessAlive(pid) {
				if replace {
					log.Printf("Replacing daemon from PID file (PID %s)...", pid)
					stopped, err := stopDaemonFromPidFile(dataDir)
					if err != nil {
						return fmt.Errorf("cannot replace daemon: %w", err)
					}
					if stopped {
						// The instance is gone; its PID file is stale. Removing it
						// is what lets the O_EXCL write below succeed without
						// waiting for the kernel to reap the old process.
						os.Remove(pidPath)
					}
				} else {
					return fmt.Errorf("zombie daemon detected (PID %s, no socket). Use --replace to force restart", pid)
				}
			}
		}
	}

	// Write our PID — O_EXCL is atomic on Linux: only one process wins
	f, err := os.OpenFile(pidPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		// Another process just wrote the PID file — check if it's alive
		time.Sleep(200 * time.Millisecond)
		pid := readPidFile(pidPath)
		if pid != "" && isProcessAlive(pid) && !isProcessZombie(pid) {
			return fmt.Errorf("daemon already starting (PID %s)", pid)
		}
		// Dead process — remove and retry atomically
		os.Remove(pidPath)
		f2, err2 := os.OpenFile(pidPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
		if err2 != nil {
			return fmt.Errorf("daemon already starting (concurrent start): %w", err2)
		}
		f2.WriteString(strconv.Itoa(os.Getpid()))
		f2.Close()
		return nil
	}
	f.WriteString(strconv.Itoa(os.Getpid()))
	f.Close()

	return nil
}

func pidFilePath(dataDir string) string {
	return dataDir + "/daemon.pid"
}

func readPidFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

func isProcessAlive(pidStr string) bool {
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks if process exists without actually signaling.
	// Note: zombies also respond to signal 0 — use isProcessZombie() to distinguish.
	return proc.Signal(syscall.Signal(0)) == nil
}

// isProcessZombie checks if a process is in zombie state (State: Z in /proc).
// Zombies respond to signal 0 (appear "alive") but cannot be killed.
func isProcessZombie(pidStr string) bool {
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "State:") {
			return strings.Contains(line, "Z (zombie)")
		}
	}
	return false
}

// daemonTermGrace is how long the stopped daemon gets to exit on SIGTERM before
// the SIGKILL follows.
const daemonTermGrace = 2 * time.Second

// daemonTermPoll is the exit-check interval inside that grace.
const daemonTermPoll = 100 * time.Millisecond

// selfBinaryPath is this process's own executable, resolved through symlinks.
// The daemon stop compares it against /proc/<pid>/exe: a process that merely
// carries "yesmem" and "daemon" in its argv (a *.test binary from a yesmem
// worktree, another installation, another data dir's instance) is not ours.
// Resolved once — a test pins it to a fixture path.
var selfBinaryPath = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return filepath.Clean(exe)
}()

// Signal seams: a test drives the SIGTERM→SIGKILL order without spawning a real
// daemon. daemonProcGone answers "is the daemon gone" and must include zombies:
// a signalled daemon stays a zombie until it is reaped, its signal-0 answer is
// still true, and the kernel has already blanked its cmdline — so asking
// isProcessAlive here would report "survived, and is not ours" for a daemon that
// died exactly as asked.
var (
	daemonProcSignal    = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
	daemonProcGone      = pidDecidedlyDead
	daemonProcGraceWait = func(d time.Duration) { time.Sleep(d) }
	daemonProcNow       = time.Now
)

// waitDaemonGone polls until the process is gone or the grace expires. A poll,
// not a single sleep: a signalled process blanks its cmdline before it is
// reapable, so an identity check during that window says "cannot confirm ours"
// for a daemon that is exiting exactly as asked.
func waitDaemonGone(pid int, grace time.Duration) bool {
	deadline := daemonProcNow().Add(grace)
	// The iteration bound is the second line of defence: the seam clock is what
	// normally ends this loop, and a frozen one must not spin forever.
	for i := 0; i <= int(grace/daemonTermPoll)+1; i++ {
		if daemonProcGone(pid) {
			return true
		}
		if !daemonProcNow().Before(deadline) {
			return false
		}
		daemonProcGraceWait(daemonTermPoll)
	}
	return daemonProcGone(pid)
}

// stopDaemonFromPidFile stops the daemon instance this data dir owns.
//
// Returns stopped=true when an instance of ours was verified and is gone.
// Returns stopped=false, err=nil when the PID file identifies nothing to stop
// (absent, unparseable, stale, or our own PID) — the caller decides whether
// "nothing to stop" is enough for what it is about to do.
// Returns an error when a live process stands in the way: a process the PID file
// names but which is not this installation's daemon (never signalled, and never
// displaced by a second daemon over the same state), or our daemon surviving
// SIGKILL.
//
// Why so narrow: the former killByProcessName killed every process whose cmdline
// contained "yesmem" and "daemon" — the systemd instance, another data dir's
// daemon, a *.test binary from a yesmem worktree. On 2026-10-17 22:52 that took
// the live daemon down, and every restart repeated it until the recovery deleted
// the running agents (YM-297). The PID file is the only identity a starting
// process can verify.
func stopDaemonFromPidFile(dataDir string) (bool, error) {
	pidStr := readPidFile(pidFilePath(dataDir))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return false, nil
	}
	if !isOwnDaemonProcess(pid) {
		if daemonProcGone(pid) {
			return false, nil // stale entry: the PID is free
		}
		return false, fmt.Errorf("PID %d in %s is alive but is not this installation's daemon (not this binary, or not started with the 'daemon' argument) — not signalling it and not taking over its state; remove %s if that PID is stale", pid, pidFilePath(dataDir), pidFilePath(dataDir))
	}
	log.Printf("Stopping daemon process from PID file (PID %d)", pid)
	// SIGTERM first: a daemon that is stopping cleanly keeps its state.
	_ = daemonProcSignal(pid, syscall.SIGTERM)
	if waitDaemonGone(pid, daemonTermGrace) {
		return true, nil
	}
	// Re-verify before the irreversible signal: the number may have been
	// recycled onto an unrelated process during the grace period.
	if !isOwnDaemonProcess(pid) {
		// The identity read is a second look at /proc, and the process can end
		// between the last poll and this line. A daemon that exits cleanly but a
		// step late is the case the report measured: the identity is no longer
		// readable, and reporting that as an error would refuse a replacement
		// whose predecessor is already gone (YM-394). The liveness answer decides
		// it, not the identity: gone means nothing to signal and nothing that
		// holds the state, so the stop succeeded. A live process that is not ours
		// still refuses the replacement.
		if daemonProcGone(pid) {
			return true, nil
		}
		return false, fmt.Errorf("PID %d did not exit within %s and no longer identifies as this installation's daemon (it may have exited and its PID been reused) — not sending SIGKILL; remove %s if that PID is stale", pid, daemonTermGrace, pidFilePath(dataDir))
	}
	log.Printf("Daemon PID %d ignored SIGTERM for %s, sending SIGKILL", pid, daemonTermGrace)
	_ = daemonProcSignal(pid, syscall.SIGKILL)
	if !waitDaemonGone(pid, daemonTermGrace) {
		return false, fmt.Errorf("daemon PID %d survived SIGKILL", pid)
	}
	return true, nil
}

// isOwnDaemonProcess reports whether pid runs this executable with "daemon" as
// its first argument. A binary that make deploy replaced mid-flight answers
// "<path> (deleted)" from /proc/<pid>/exe; the suffix is stripped so a deploy
// can still replace its own daemon.
func isOwnDaemonProcess(pid int) bool {
	if selfBinaryPath == "" {
		return false
	}
	exe, err := os.Readlink(fmt.Sprintf("%s/%d/exe", procRoot, pid))
	if err != nil {
		return false
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	if filepath.Clean(exe) != selfBinaryPath {
		return false
	}
	argv := strings.Split(procCmdline(pid), "\x00")
	return len(argv) > 1 && argv[1] == "daemon"
}

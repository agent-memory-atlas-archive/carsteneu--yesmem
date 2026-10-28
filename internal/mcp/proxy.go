package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/daemon"
)

// mcpErrorLog writes structured error entries to mcp-error.log.
// This captures broken pipes, reconnect failures, and other MCP issues
// that would otherwise be lost (MCP stderr goes to /dev/null).
var mcpErrorLog *log.Logger

func initErrorLog(dataDir string) {
	logDir := filepath.Join(dataDir, "logs")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "mcp-error.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	mcpErrorLog = log.New(f, "", log.LstdFlags)
}

func logMCPError(event string, method string, err error) {
	if mcpErrorLog == nil {
		return
	}
	// Capture caller location
	_, file, line, _ := runtime.Caller(1)
	file = filepath.Base(file)
	mcpErrorLog.Printf("pid=%d %s:%d event=%s method=%q error=%v",
		os.Getpid(), file, line, event, method, err)
}

// ProxyClient connects to the daemon's Unix socket.
type ProxyClient struct {
	client  *daemon.SocketClient
	dataDir string
}

// NewProxy creates a proxy to the daemon, starting it if necessary.
func NewProxy(dataDir string) (*ProxyClient, error) {
	// Try connecting to existing daemon
	client, err := daemon.Dial(dataDir)
	if err == nil {
		// Verify connection with ping
		result, err := client.Call("ping", nil)
		if err == nil {
			var pong string
			json.Unmarshal(result, &pong)
			if pong == "pong" {
				log.Println("Connected to running daemon")
				return &ProxyClient{client: client, dataDir: dataDir}, nil
			}
		}
		client.Close()
	}

	// Daemon not running — start it
	log.Println("Daemon not running, starting...")
	if err := startDaemon(dataDir); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}

	// Wait for daemon to be ready (up to 120s — with socket-first this takes ~2s)
	newClient, err := dialWithTimeout(dataDir, 120*time.Second)
	if err != nil {
		return nil, fmt.Errorf("daemon did not become ready: %w", err)
	}
	log.Println("Daemon started and ready")
	return &ProxyClient{client: newClient, dataDir: dataDir}, nil
}

// Call sends a request to the daemon, retrying once on connection error.
// Daemon-level errors (Response.Error) are returned directly without reconnect.
func (p *ProxyClient) Call(method string, params map[string]any) (json.RawMessage, error) {
	result, err := p.callOnce(method, params)
	if err == nil {
		return result, nil
	}

	// Daemon-level errors are application errors, not connection failures — don't reconnect
	if isDaemonError(err) {
		return nil, err
	}

	// Connection error — log and try reconnecting once (5s timeout)
	logMCPError("broken_pipe", method, err)
	if p.client != nil {
		p.client.Close()
	}

	newClient, dialErr := dialWithTimeout(p.dataDir, 5*time.Second)
	if dialErr != nil {
		logMCPError("reconnect_failed", method, fmt.Errorf("%w (original: %v)", dialErr, err))
		return nil, fmt.Errorf("reconnect failed: %w (original: %v)", dialErr, err)
	}
	p.client = newClient
	logMCPError("reconnect_ok", method, nil)

	// Retry the original call
	retryResult, retryErr := p.callOnce(method, params)
	if retryErr != nil && !isDaemonError(retryErr) {
		logMCPError("retry_failed", method, retryErr)
	}
	return retryResult, retryErr
}

// isDaemonError returns true if the error is a daemon-level application error
// (not a connection/transport error). These should not trigger reconnection.
func isDaemonError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "daemon: ")
}

// CallString convenience method — returns result as string.
func (p *ProxyClient) CallString(method string, params map[string]any) (string, error) {
	result, err := p.Call(method, params)
	if err != nil {
		return "", err
	}
	return string(result), nil
}

// Close closes the proxy connection.
func (p *ProxyClient) Close() error {
	return p.client.Close()
}

func (p *ProxyClient) callOnce(method string, params map[string]any) (json.RawMessage, error) {
	client, err := daemon.DialTimeout(p.dataDir, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	if isFastMCPMethod(method) {
		_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	}

	return client.Call(method, params)
}

func isFastMCPMethod(method string) bool {
	switch method {
	case "ping", "search", "deep_search", "hybrid_search", "docs_search", "related_to_file", "list_projects", "get_learnings", "get_config":
		return true
	default:
		return false
	}
}

// dialWithTimeout tries to connect to daemon with a short timeout (for reconnects).
func dialWithTimeout(dataDir string, timeout time.Duration) (*daemon.SocketClient, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client, err := daemon.Dial(dataDir)
		if err == nil {
			result, err := client.Call("ping", nil)
			if err == nil {
				var pong string
				json.Unmarshal(result, &pong)
				if pong == "pong" {
					log.Println("Reconnected to daemon")
					return client, nil
				}
			}
			client.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon not reachable within %v", timeout)
}

// errDaemonOwnedBySystemd marks the case where the user unit owns the daemon and
// its socket did not answer. The caller fails fast on it instead of waiting for a
// socket that nothing is going to create.
var errDaemonOwnedBySystemd = errors.New("yesmem.service owns the daemon")

// yesmemUnitState is what systemd knows about this installation's user unit.
type yesmemUnitState int

const (
	// yesmemUnitAbsent: no user unit installed, so the unit cannot own the
	// daemon and a client has to spawn the binary itself.
	yesmemUnitAbsent yesmemUnitState = iota
	yesmemUnitActive
	yesmemUnitInactive
)

// daemonStartDeps are the four things startDaemonWith needs from its
// environment. A test supplies fakes, so the decision is checked without a real
// systemctl, without spawning a binary and without a live unit.
type daemonStartDeps struct {
	unitState  func() yesmemUnitState
	systemctl  func(args ...string) error
	executable func() (string, error)
	spawn      func(dataDir, exe string, args, env []string) error
}

// defaultDaemonStartDeps is the production wiring.
var defaultDaemonStartDeps = daemonStartDeps{
	unitState:  detectYesmemUnitState,
	systemctl:  systemctlUser,
	executable: os.Executable,
	spawn:      spawnDaemonProcess,
}

// unitStateTimeout bounds the systemd probe: a wedged user bus must not hang the
// MCP call that asked for the daemon.
const unitStateTimeout = 5 * time.Second

// systemctlBinary resolves the systemctl the client executes.
//
// The bare name left the choice to PATH, and PATH carries user-writable
// directories on a normal install (~/.local/bin is prepended by the daemon, and
// the installer puts yesmem there). This refuses a candidate that is not a
// regular, executable file or that any local writer may replace — same heuristic
// as sqlite3Binary in internal/daemon/opencode_capture.go.
//
// What it does not do (same limits as sqlite3Binary): the file's owner and its
// directory are not checked, so a user-owned 0755 file in a user-writable PATH
// entry still passes, and a window remains between this check and the exec. It
// keeps a sloppy PATH from deciding what the client runs; it does not stop a
// local attacker who already controls HOME.
func systemctlBinary() (string, error) {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return "", fmt.Errorf("systemctl not found: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve systemctl %q: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat systemctl %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("systemctl %q is not a regular file", resolved)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("systemctl %q is not executable", resolved)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("systemctl %q is writable by group or others — refusing to execute a binary any local writer can replace", resolved)
	}
	return resolved, nil
}

// detectYesmemUnitState asks systemd whether yesmem.service is installed and
// running. A failure to ask (no user bus, no systemd, a timeout, an untrusted
// systemctl) reads as "absent": the client then falls back to handling the
// daemon itself.
func detectYesmemUnitState() yesmemUnitState {
	bin, err := systemctlBinary()
	if err != nil {
		log.Printf("cannot inspect yesmem.service (%v) — treating it as not installed", err)
		return yesmemUnitAbsent
	}
	ctx, cancel := context.WithTimeout(context.Background(), unitStateTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--user", "show", "-p", "LoadState", "-p", "ActiveState", "yesmem.service").Output()
	if err != nil {
		return yesmemUnitAbsent
	}
	return parseYesmemUnitState(string(out))
}

func parseYesmemUnitState(out string) yesmemUnitState {
	load, active := "", ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "LoadState="); ok {
			load = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "ActiveState="); ok {
			active = strings.TrimSpace(v)
		}
	}
	if load != "loaded" {
		return yesmemUnitAbsent
	}
	if active == "active" || active == "activating" || active == "reloading" {
		return yesmemUnitActive
	}
	return yesmemUnitInactive
}

// systemctlStartTimeout bounds the start call: the probe next to it is bounded,
// and a wedged user bus must not block the MCP startup indefinitely.
const systemctlStartTimeout = 30 * time.Second

func systemctlUser(args ...string) error {
	bin, err := systemctlBinary()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), systemctlStartTimeout)
	defer cancel()
	return exec.CommandContext(ctx, bin, append([]string{"--user"}, args...)...).Run()
}

// spawnDaemonLogFile is where a self-spawned daemon's own refusals land. Its
// stderr used to go to /dev/null, so "daemon already running" or "zombie daemon
// detected" never reached anyone and the client could only report a timeout.
func spawnDaemonLogFile(dataDir string) (*os.File, error) {
	dir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "daemon-spawn.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
}

func spawnDaemonProcess(dataDir, exe string, args, env []string) error {
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	// Detach from the MCP process so the daemon survives its exit. A log file is
	// kept open on purpose: closing it here would cut the child's stderr off.
	if logFile, err := spawnDaemonLogFile(dataDir); err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon process: %w", err)
	}
	return cmd.Process.Release()
}

func startDaemon(dataDir string) error {
	// Fork-bomb guard: under `go test` os.Executable() resolves to the
	// *.test binary, not yesmem. Spawning that with "daemon --replace"
	// re-runs the entire test suite in the child, which calls mcp.New()
	// again, which lands here, which forks again — earlyoom kills the
	// machine before the kernel can. Refuse early and let the caller
	// (NewProxy → New) continue with proxy=nil, which is harmless for
	// tests that only need the registered tool schema.
	if testing.Testing() {
		return fmt.Errorf("startDaemon refused under go test (os.Executable() points to *.test → fork bomb risk); construct *Server manually if you need the daemon path")
	}
	return startDaemonWith(dataDir, defaultDaemonStartDeps)
}

// startDaemonWith is startDaemon without the go-test guard, for a test that
// drives the decision with fake dependencies.
//
// A client never replaces a daemon it cannot see. The former body spawned
// "yesmem daemon --replace" unconditionally, so an MCP client whose socket
// appeared dead killed the running instance and every restart repeated it — the
// 2026-10-17 cascade (YM-297). The unit is the owner when it exists: an active
// unit is left to systemd (the caller waits for the socket), a stopped unit is
// started through systemctl, and only an installation without a unit lets this
// process spawn a daemon — without --replace, so a daemon that is merely
// unreachable stays alive.
func startDaemonWith(dataDir string, deps daemonStartDeps) error {
	switch deps.unitState() {
	case yesmemUnitActive:
		// The unit owns the daemon; a second one started here would be the
		// YM-297 cascade again. Reported as an error so the caller does not
		// spend the whole dial timeout waiting for a socket nothing is creating.
		log.Println("Daemon socket unreachable while yesmem.service is active — leaving the restart to systemd")
		return fmt.Errorf("%w: its socket did not answer — run 'systemctl --user restart yesmem'", errDaemonOwnedBySystemd)
	case yesmemUnitInactive:
		log.Println("Daemon socket unreachable — starting yesmem.service via systemctl --user")
		if err := deps.systemctl("start", "yesmem"); err != nil {
			return fmt.Errorf("start yesmem.service: %w", err)
		}
		return nil
	}

	// Find the yesmem binary
	exe, err := deps.executable()
	if err != nil {
		return err
	}

	// No projects directory is passed: the daemon resolves it from config.yaml
	// itself (main.go). A second derivation in the environment was never read and
	// could only drift from the real one, so the entry is gone.
	return deps.spawn(dataDir, exe, []string{"daemon"}, append(os.Environ(),
		"YESMEM_DATA_DIR="+dataDir,
	))
}

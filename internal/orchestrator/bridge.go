package orchestrator

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"

	pty "github.com/creack/pty/v2"
)

// AgentBridge holds a PTY master and bridges it to a Unix socket.
// The terminal-side program (agent-tty) connects to the socket.
// The Go tool can inject messages via Inject() or the inject socket.
type AgentBridge struct {
	Ptmx       *os.File     // PTY master — Go holds this
	SockPath   string       // Unix socket path for agent-tty
	InjectPath string       // Unix socket path for inject/relay
	Cmd        *exec.Cmd    // Claude process
	listener   net.Listener // Socket listener (agent-tty)
	injectLn   net.Listener // Socket listener (inject)
	mu         sync.Mutex   // Protects Ptmx writes
}

// NewAgentBridge creates a PTY, starts the command on it, and prepares a Unix socket.
// If workDir is non-empty, the command runs in that directory.
// extraEnv are additional environment variables (e.g. OPENAI_API_KEY for codex).
func NewAgentBridge(name string, args []string, sockPath string, workDir string, extraEnv ...string) (*AgentBridge, error) {
	// net.Listen("unix", "") binds an ABSTRACT socket and reports no error, so an
	// empty path used to produce a bridge that no console can ever reach. Fail
	// loudly instead: the callers derive the path from the agent id (YM-287).
	if sockPath == "" {
		return nil, fmt.Errorf("empty socket path")
	}
	cmd := exec.Command(name, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	// Inherit full environment and ensure terminal colors work
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor", "FORCE_COLOR=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty.Start: %w", err)
	}

	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 50, Cols: 100})

	injectPath := sockPath + ".inject"
	// teardown releases a half-built bridge: every listener bound so far, both
	// socket files, the pty and the child process, which is reaped so an aborted
	// spawn cannot leave a zombie behind.
	teardown := func(listeners ...net.Listener) {
		for _, ln := range listeners {
			ln.Close()
		}
		os.Remove(sockPath)
		os.Remove(injectPath)
		ptmx.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}

	os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		teardown()
		return nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}
	if err := hardenSocket(sockPath); err != nil {
		teardown(ln)
		return nil, err
	}

	// Inject socket stays open for relay commands
	os.Remove(injectPath)
	injectLn, err := net.Listen("unix", injectPath)
	if err != nil {
		teardown(ln)
		return nil, fmt.Errorf("listen %s: %w", injectPath, err)
	}
	if err := hardenSocket(injectPath); err != nil {
		teardown(ln, injectLn)
		return nil, err
	}

	// The socket paths belong to the agent id, not to this process: a resume or a
	// restart binds a successor on the same paths, so neither listener may unlink
	// them on close (net.Listen unlinks by default). Whether the files still
	// belong to this process is a decision for its owner — Close vs
	// CloseKeepingSockets — and the listener cannot know it (YM-70).
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if ul, ok := injectLn.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}

	return &AgentBridge{Ptmx: ptmx, SockPath: sockPath, InjectPath: injectPath, Cmd: cmd, listener: ln, injectLn: injectLn}, nil
}

// hardenSocket makes a freshly bound agent socket owner-only. net.Listen applies
// the umask, so on a host with umask 002 both files land as 0775: the agent
// socket bridges the console to the PTY and the inject socket writes straight
// into the agent's terminal, so a group or world writable file there lets every
// local account type commands into an unsandboxed agent session (audit
// 16.10.2026, YM-328). Every client (agent-tty console, relay_agent, the daemon)
// runs as the same user, so owner-only costs no reachability — the same
// reasoning as daemon.sock since YM-250.
func hardenSocket(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// Serve blocks and bridges the first socket connection to the PTY.
// Also starts the inject listener for relay commands.
// Call in a goroutine.
//
// Serve closes the listeners when the process ends but never unlinks the socket
// files: the paths are per agent id, so they may already belong to a successor
// process. The removal is made by the owner, which knows whether the row still
// names the process that created them (Close vs CloseKeepingSockets).
func (b *AgentBridge) Serve() {
	defer b.listener.Close()
	defer b.injectLn.Close()

	// Start inject listener in background
	go b.serveInject()

	conn, err := b.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	done := make(chan struct{}, 2)

	go func() {
		b.copyWithWinsize(conn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(conn, b.Ptmx)
		done <- struct{}{}
	}()

	<-done
}

// serveInject accepts short-lived connections on the inject socket.
// Each connection's content is written to the PTY master as user input.
func (b *AgentBridge) serveInject() {
	for {
		conn, err := b.injectLn.Accept()
		if err != nil {
			return // listener closed
		}
		go func(c net.Conn) {
			defer c.Close()
			data, err := io.ReadAll(c)
			if err == nil && len(data) > 0 {
				b.mu.Lock()
				b.Ptmx.Write(data)
				b.mu.Unlock()
			}
		}(conn)
	}
}

// copyWithWinsize reads from the socket, intercepts winsize markers (0x01),
// and forwards everything else to the PTY master.
func (b *AgentBridge) copyWithWinsize(conn net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			data := buf[:n]
			for len(data) > 0 {
				if data[0] == 0x01 && len(data) >= 5 {
					rows := binary.BigEndian.Uint16(data[1:3])
					cols := binary.BigEndian.Uint16(data[3:5])
					pty.Setsize(b.Ptmx, &pty.Winsize{Rows: rows, Cols: cols})
					data = data[5:]
				} else {
					// Find next potential marker or write all
					end := 1
					for end < len(data) && data[end] != 0x01 {
						end++
					}
					b.mu.Lock()
					b.Ptmx.Write(data[:end])
					b.mu.Unlock()
					data = data[end:]
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// Inject writes a message directly into the PTY master.
// Claude sees it as keyboard input. Thread-safe.
func (b *AgentBridge) Inject(msg string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.Ptmx.Write([]byte(msg))
	return err
}

// Close cleans up the bridge: it detaches from the PTY and removes the socket
// files.
func (b *AgentBridge) Close() {
	b.close(true)
}

// CloseKeepingSockets detaches from the PTY without unlinking the socket paths.
// The paths are per agent id, so after a resume or a restart the files at them
// belong to the successor process — a stale process tearing its own sockets down
// would take the live agent's socket with them (YM-70).
func (b *AgentBridge) CloseKeepingSockets() {
	b.close(false)
}

func (b *AgentBridge) close(unlinkSockets bool) {
	b.listener.Close()
	b.injectLn.Close()
	b.Ptmx.Close()
	if !unlinkSockets {
		return
	}
	os.Remove(b.SockPath)
	os.Remove(b.InjectPath)
}

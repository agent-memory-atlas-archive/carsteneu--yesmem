package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/carsteneu/yesmem/internal/terminals"
)

// agentTTYOpts are the parsed `yesmem agent-tty` flags.
type agentTTYOpts struct {
	sock         string
	parentPid    int
	parentWindow string
}

// wantsPlacement reports whether there is a caller desktop to inherit: either
// the caller's pid chain or the window registered for the caller's session.
func (o agentTTYOpts) wantsPlacement() bool {
	return o.parentPid > 0 || o.parentWindow != ""
}

func parseAgentTTYFlags(args []string) (agentTTYOpts, error) {
	var o agentTTYOpts
	fs := flag.NewFlagSet("agent-tty", flag.ContinueOnError)
	// Silent: runAgentTTY reports the error exactly once.
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.sock, "sock", "", "Unix socket path to connect to")
	fs.IntVar(&o.parentPid, "parent-pid", 0, "PID of the caller; moves this window onto its desktop")
	fs.StringVar(&o.parentWindow, "parent-window", "", "X11 window id registered for the caller's session; preferred over --parent-pid")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.sock == "" {
		return o, errNoSock
	}
	return o, nil
}

const agentTTYUsage = "usage: yesmem agent-tty --sock <path> [--parent-pid <pid>] [--parent-window <0xid>]"

var errNoSock = errors.New("--sock is required")

// agentTTYExitCode keeps the exit codes of the former flag.ExitOnError set:
// 0 for -h, 2 for a flag parse error, 1 for a missing --sock.
func agentTTYExitCode(err error) int {
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errNoSock):
		return 1
	default:
		return 2
	}
}

func runAgentTTY() {
	opts, err := parseAgentTTYFlags(os.Args[2:])
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(agentTTYUsage)
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n%s\n", err, agentTTYUsage)
		os.Exit(agentTTYExitCode(err))
	}

	if opts.wantsPlacement() {
		// Land next to the terminal this agent was spawned from instead of on
		// whichever desktop is focused. Off the critical path: the bridge must
		// come up regardless of whether the window can be moved.
		go terminals.PlaceSelfOnCallerDesktop(opts.parentWindow, opts.parentPid, 6*time.Second)
	}

	conn, err := net.Dial("unix", opts.sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect %s: %v\n", opts.sock, err)
		os.Exit(1)
	}
	defer conn.Close()

	// Send initial terminal size (4 bytes: rows u16 + cols u16)
	sendWinsize(conn)

	// Set terminal to raw mode so keystrokes pass through immediately
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err == nil {
		defer term.Restore(fd, oldState)
	}

	// Handle SIGWINCH — forward terminal resize to bridge
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			sendWinsize(conn)
		}
	}()

	// Handle Ctrl-C gracefully — restore terminal before exit
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		if oldState != nil {
			term.Restore(fd, oldState)
		}
		os.Exit(0)
	}()

	// Bidirectional bridge: terminal ↔ socket ↔ PTY ↔ claude
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(conn, os.Stdin)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(os.Stdout, conn)
		done <- struct{}{}
	}()
	<-done
}

// sendWinsize sends the current terminal size over the connection.
// Protocol: 1 byte marker (0x01) + 2 bytes rows + 2 bytes cols (big-endian).
func sendWinsize(conn net.Conn) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	buf := make([]byte, 5)
	buf[0] = 0x01 // winsize marker
	binary.BigEndian.PutUint16(buf[1:3], uint16(h))
	binary.BigEndian.PutUint16(buf[3:5], uint16(w))
	conn.Write(buf)
}

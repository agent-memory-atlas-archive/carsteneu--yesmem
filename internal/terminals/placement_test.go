package terminals

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// x11Stub replaces commandOut, procPPID and selfPID for the duration of one test.
type x11Stub struct {
	windows string      // stdout of `wmctrl -lp`
	display bool        // whether `wmctrl -m` succeeds
	lpFail  bool        // whether `wmctrl -lp` fails (no display)
	chain   map[int]int // child pid -> parent pid
	self    int         // pid reported as "the process in the new window"
	placed  [][]string  // recorded args of every `wmctrl -i -r <xid> -t <desk>`
	calls   int         // number of wmctrl invocations
}

func stubX11(t *testing.T, s *x11Stub) {
	t.Helper()
	origOut, origPPID, origSelf := commandOut, procPPID, selfPID
	t.Cleanup(func() { commandOut, procPPID, selfPID = origOut, origPPID, origSelf })

	s.placed = nil
	commandOut = func(name string, args ...string) (string, error) {
		if name != "wmctrl" {
			return "", fmt.Errorf("unexpected binary %q", name)
		}
		s.calls++
		switch {
		case len(args) == 1 && args[0] == "-m":
			if !s.display {
				return "", errors.New("no display")
			}
			return "0  0  0  5120  1400  host  Desktop", nil
		case len(args) == 1 && args[0] == "-lp":
			if s.lpFail {
				return "", errors.New("Cannot open display")
			}
			return s.windows, nil
		case len(args) == 5 && args[0] == "-i":
			s.placed = append(s.placed, args)
			return "", nil
		}
		return "", fmt.Errorf("unexpected wmctrl %v", args)
	}
	procPPID = func(pid int) (int, error) {
		p, ok := s.chain[pid]
		if !ok {
			return 0, fmt.Errorf("no parent for pid %d", pid)
		}
		return p, nil
	}
	if s.self != 0 {
		selfPID = func() int { return s.self }
	}
}

// callerWindows is the window of the terminal the caller works in.
const callerWindow = "0x04c00004 11 1626101 chieftp32 OC | # Conversation Title"

// agentWindow is the window of the freshly spawned agent terminal, which the
// window manager put on the focused desktop 0.
const agentWindow = "0x07770004 0 555001 chieftp32 yesmem-task-a #7"

// callerChain ends on 1626101, the pid owning callerWindow.
var callerChain = map[int]int{900: 750, 750: 1626101, 1626101: 1}

func TestParseWmctrlPidLine(t *testing.T) {
	line := "0x04a00004 10 9046 chieftp32 OC | I'll search my memory"
	w, ok := ParseWmctrlPidLine(line)
	if !ok {
		t.Fatal("expected parse ok")
	}
	// Felder: id desktop pid host title...
	if w.XID != "0x4a00004" || w.Workspace != 10 || w.Pid != 9046 {
		t.Fatalf("parsed %+v", w)
	}
	// A window without a title still yields four fields.
	if w, ok := ParseWmctrlPidLine("0x02e00003 13 34681 chieftp32"); !ok || w.Pid != 34681 || w.Workspace != 13 {
		t.Fatalf("title-less line: %+v ok=%v", w, ok)
	}
	for _, bad := range []string{
		"",
		"# header",
		"0x1 0",
		"0x1 0 notanumber host title",
		"0x1 notanumber 5 host title",
	} {
		if _, ok := ParseWmctrlPidLine(bad); ok {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}

func TestDesktopForProcess_WalksAncestors(t *testing.T) {
	st := &x11Stub{
		display: true,
		windows: "0x05c00004 12 83898 chieftp32 ~/projects/opencode\n" + callerWindow,
		// 900 is the calling CLI, 750 its shell, 83898 the ghostty owning the
		// window that shell runs in.
		chain: map[int]int{900: 750, 750: 83898, 83898: 1},
	}
	stubX11(t, st)

	got, ok := DesktopForProcess(900)
	if !ok || got != 12 {
		t.Fatalf("DesktopForProcess(900) = %d, %v; want 12, true", got, ok)
	}
}

func TestDesktopForProcess_OwnWindowWins(t *testing.T) {
	st := &x11Stub{
		display: true,
		windows: "0x05c00004 12 83898 chieftp32 x",
		chain:   map[int]int{83898: 1},
	}
	stubX11(t, st)

	if got, ok := DesktopForProcess(83898); !ok || got != 12 {
		t.Fatalf("DesktopForProcess(83898) = %d, %v; want 12, true", got, ok)
	}
}

func TestDesktopForProcess_NoWindowInChain(t *testing.T) {
	st := &x11Stub{
		display: true,
		windows: "0x05c00004 12 83898 chieftp32 x",
		chain:   map[int]int{900: 750, 750: 1},
	}
	stubX11(t, st)

	if got, ok := DesktopForProcess(900); ok {
		t.Fatalf("DesktopForProcess(900) = %d, true; want not ok", got)
	}
}

func TestDesktopForProcess_StopsOnChainBreak(t *testing.T) {
	st := &x11Stub{display: true, chain: map[int]int{900: 750}}
	stubX11(t, st)

	if _, ok := DesktopForProcess(900); ok {
		t.Fatal("a broken parent chain must not resolve")
	}
}

func TestDesktopForProcess_RejectsInvalidPid(t *testing.T) {
	st := &x11Stub{display: true}
	stubX11(t, st)

	if _, ok := DesktopForProcess(0); ok {
		t.Fatal("pid 0 must not resolve")
	}
}

func TestWaitOwnerWindow(t *testing.T) {
	st := &x11Stub{display: true, windows: callerWindow, chain: callerChain}
	stubX11(t, st)

	// 1626101 owns the window directly.
	w, ok := WaitOwnerWindow(1626101, time.Second)
	if !ok || w.XID != "0x4c00004" || w.Workspace != 11 {
		t.Fatalf("WaitOwnerWindow = %+v, %v; want 0x4c00004 on 11", w, ok)
	}
	// 900 reaches it through the chain.
	if w, ok := WaitOwnerWindow(900, time.Second); !ok || w.XID != "0x4c00004" {
		t.Fatalf("via ancestor: %+v, %v", w, ok)
	}
	// A pid whose chain holds no window gives up after the timeout.
	if _, ok := WaitOwnerWindow(4242, 300*time.Millisecond); ok {
		t.Fatal("unknown pid must not resolve")
	}
}

func TestPlaceSelfOnParentDesktop(t *testing.T) {
	st := &x11Stub{
		display: true,
		windows: callerWindow + "\n" + agentWindow,
		// 700001 is this very process; 555001 is the terminal owning its window.
		chain: map[int]int{700001: 555001, 555001: 1, 900: 750, 750: 1626101, 1626101: 1},
		self:  700001,
	}
	stubX11(t, st)

	// The caller's terminal sits on desktop 11; the agent window was opened on
	// the focused desktop 0 and must be moved.
	got := PlaceSelfOnParentDesktop(900, time.Second)
	if got != 11 {
		t.Fatalf("placed on %d, want 11", got)
	}
	if len(st.placed) != 1 {
		t.Fatalf("want exactly one placement call, got %d: %v", len(st.placed), st.placed)
	}
	want := []string{"-i", "-r", "0x7770004", "-t", "11"}
	if !reflect.DeepEqual(st.placed[0], want) {
		t.Fatalf("placement args = %v, want %v", st.placed[0], want)
	}
}

func TestDesktopForWindow(t *testing.T) {
	st := &x11Stub{display: true, windows: callerWindow + "\n" + agentWindow}
	stubX11(t, st)

	// Zero-padded form as sent by the opencode TUI plugin ("0x%08x").
	if got, ok := DesktopForWindow("0x04c00004"); !ok || got != 11 {
		t.Fatalf("DesktopForWindow(padded) = %d, %v; want 11, true", got, ok)
	}
	// Normalised form as printed by wmctrl after normXID.
	if got, ok := DesktopForWindow("0x4C00004"); !ok || got != 11 {
		t.Fatalf("DesktopForWindow(unpadded upper) = %d, %v; want 11, true", got, ok)
	}
	for _, xid := range []string{"", "0x0abcdef", "not-a-window"} {
		if got, ok := DesktopForWindow(xid); ok {
			t.Errorf("DesktopForWindow(%q) = %d, true; want not ok", xid, got)
		}
	}
}

func TestPlaceSelfOnCallerDesktop_RegisteredWindowWins(t *testing.T) {
	st := &x11Stub{
		display: true,
		// The pid chain of 900 reaches a different window on desktop 12; the
		// window registered for the caller's session sits on desktop 11.
		windows: callerWindow + "\n" + agentWindow + "\n0x05c00004 12 83898 chieftp32 x",
		chain:   map[int]int{700001: 555001, 555001: 1, 900: 83898, 83898: 1},
		self:    700001,
	}
	stubX11(t, st)

	if got := PlaceSelfOnCallerDesktop("0x04c00004", 900, time.Second); got != 11 {
		t.Fatalf("placed on %d, want 11 (registered window)", got)
	}
	want := []string{"-i", "-r", "0x7770004", "-t", "11"}
	if len(st.placed) != 1 || !reflect.DeepEqual(st.placed[0], want) {
		t.Fatalf("placement calls = %v, want [%v]", st.placed, want)
	}
}

func TestPlaceSelfOnCallerDesktop_SharedServerWindowOnly(t *testing.T) {
	// Session hosted by the shared `opencode serve`: the caller pid chain
	// (serve -> systemd) holds no window, only the registered window answers.
	st := &x11Stub{
		display: true,
		windows: callerWindow + "\n" + agentWindow,
		chain:   map[int]int{700001: 555001, 555001: 1, 1706879: 1579, 1579: 1},
		self:    700001,
	}
	stubX11(t, st)

	if got := PlaceSelfOnCallerDesktop("0x04c00004", 1706879, time.Second); got != 11 {
		t.Fatalf("placed on %d, want 11", got)
	}
}

func TestPlaceSelfOnCallerDesktop_FallsBackToPid(t *testing.T) {
	// The registered window was closed meanwhile: fall back to the pid walk.
	st := &x11Stub{
		display: true,
		windows: callerWindow + "\n" + agentWindow,
		chain:   map[int]int{700001: 555001, 555001: 1, 900: 750, 750: 1626101, 1626101: 1},
		self:    700001,
	}
	stubX11(t, st)

	if got := PlaceSelfOnCallerDesktop("0x0dead004", 900, time.Second); got != 11 {
		t.Fatalf("placed on %d, want 11 via pid fallback", got)
	}
}

func TestPlaceSelfOnCallerDesktop_NoopCases(t *testing.T) {
	cases := []struct {
		name   string
		stub   *x11Stub
		window string
		pid    int
	}{
		{"headless", &x11Stub{display: false, windows: callerWindow, chain: callerChain, self: 700001}, "0x04c00004", 900},
		{"nothing to inherit", &x11Stub{display: true, windows: callerWindow, chain: callerChain, self: 700001}, "", 0},
		{"window gone and pid chain empty", &x11Stub{display: true, windows: callerWindow, chain: map[int]int{900: 1}, self: 700001}, "0x0dead004", 900},
		{"own window never appears", &x11Stub{display: true, windows: callerWindow, chain: callerChain, self: 4242}, "0x04c00004", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubX11(t, c.stub)
			if got := PlaceSelfOnCallerDesktop(c.window, c.pid, 300*time.Millisecond); got != -1 {
				t.Fatalf("got %d, want -1", got)
			}
			if len(c.stub.placed) != 0 {
				t.Fatalf("no placement expected, got %v", c.stub.placed)
			}
		})
	}
}

func TestPlaceSelfOnParentDesktop_NoopCases(t *testing.T) {
	cases := []struct {
		name      string
		stub      *x11Stub
		parentPid int
	}{
		{
			name:      "headless daemon",
			stub:      &x11Stub{display: false, windows: callerWindow, chain: callerChain, self: 700001},
			parentPid: 900,
		},
		{
			name:      "caller chain has no window",
			stub:      &x11Stub{display: true, windows: callerWindow, chain: map[int]int{900: 750, 750: 1}, self: 700001},
			parentPid: 900,
		},
		{
			name:      "own window never appears",
			stub:      &x11Stub{display: true, windows: callerWindow, chain: callerChain, self: 4242},
			parentPid: 900,
		},
		{
			name:      "no caller pid (nothing to inherit)",
			stub:      &x11Stub{display: true, windows: callerWindow, chain: callerChain, self: 700001},
			parentPid: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubX11(t, c.stub)
			if got := PlaceSelfOnParentDesktop(c.parentPid, 300*time.Millisecond); got != -1 {
				t.Fatalf("got %d, want -1", got)
			}
			if len(c.stub.placed) != 0 {
				t.Fatalf("no placement expected, got %v", c.stub.placed)
			}
		})
	}
}

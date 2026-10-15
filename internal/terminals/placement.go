package terminals

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maxAncestorHops bounds the parent-chain walk in ownerWindow so a corrupted
// /proc view cannot loop forever.
const maxAncestorHops = 16

// PidWindow is one line of `wmctrl -lp`: a mapped window together with the PID
// of the process that owns it and the desktop it currently sits on.
type PidWindow struct {
	XID       string
	Workspace int
	Pid       int
}

// selfPID is the process whose own window belongs to this call. Indirected
// through a var so tests can stub the process tree.
var selfPID = os.Getpid

// procPPID reads the parent PID from /proc/<pid>/stat. Indirected through a var
// so tests can stub the process tree. The comm field (2) may contain spaces and
// parentheses, so the ppid is read from after the LAST ')'.
var procPPID = func(pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	return strconv.Atoi(fields[1])
}

// ParseWmctrlPidLine parses one `wmctrl -lp` line into a PidWindow.
// Field layout (whitespace-separated):
//
//	<id> <desktop> <pid> <host> <title...>
//
// Unlike `wmctrl -lG` this form carries no geometry but the owning PID, which is
// what maps a process to the window it runs in. A window without a title still
// yields four fields.
func ParseWmctrlPidLine(line string) (PidWindow, bool) {
	f := strings.Fields(line)
	if len(f) < 4 {
		return PidWindow{}, false
	}
	desk, errDesk := strconv.Atoi(f[1])
	pid, errPid := strconv.Atoi(f[2])
	if errDesk != nil || errPid != nil {
		return PidWindow{}, false
	}
	return PidWindow{XID: normXID(f[0]), Workspace: desk, Pid: pid}, true
}

// LiveWindows lists every mapped window (canonical id, desktop, owner pid)
// with a single `wmctrl -lp` call. err != nil means "can't tell" — no display
// or wmctrl failed — and callers must not treat any window as closed then.
func LiveWindows() ([]PidWindow, error) {
	out, err := commandOut("wmctrl", "-lp")
	if err != nil {
		return nil, err
	}
	var res []PidWindow
	for _, line := range strings.Split(out, "\n") {
		if w, ok := ParseWmctrlPidLine(line); ok {
			res = append(res, w)
		}
	}
	return res, nil
}

// pidWindows lists every mapped window with its owner PID and desktop.
func pidWindows() []PidWindow {
	out, err := commandOut("wmctrl", "-lp")
	if err != nil {
		return nil
	}
	var res []PidWindow
	for _, line := range strings.Split(out, "\n") {
		if w, ok := ParseWmctrlPidLine(line); ok {
			res = append(res, w)
		}
	}
	return res
}

// ownerWindow returns the window owned by pid or by its nearest ancestor that
// owns one. ok is false when the chain holds no window.
//
// With gtk-single-instance disabled — the configuration this install runs —
// the terminal emulator owning a window is a direct ancestor of every process
// started inside it, so walking /proc upwards reliably reaches the window a
// process was started in. If one PID owns several windows only the first is
// considered.
func ownerWindow(pid int) (PidWindow, bool) {
	if pid <= 0 {
		return PidWindow{}, false
	}
	wins := pidWindows()
	for hops := 0; hops < maxAncestorHops && pid > 1; hops++ {
		for _, w := range wins {
			if w.Pid == pid {
				return w, true
			}
		}
		next, err := procPPID(pid)
		if err != nil || next == pid {
			break
		}
		pid = next
	}
	return PidWindow{}, false
}

// DesktopForProcess returns the desktop of the window owned by pid or by its
// nearest ancestor that owns one.
func DesktopForProcess(pid int) (int, bool) {
	w, ok := ownerWindow(pid)
	if !ok {
		return 0, false
	}
	return w.Workspace, true
}

// DesktopForWindow returns the desktop a window currently sits on, read live
// from the window manager. ok is false when the window is not mapped (closed,
// or the id is malformed). Padded ("0x04c00004") and unpadded ids match.
func DesktopForWindow(xid string) (int, bool) {
	if !strings.HasPrefix(strings.ToLower(xid), "0x") {
		return 0, false
	}
	want := normXID(xid)
	for _, w := range pidWindows() {
		if w.XID == want {
			return w.Workspace, true
		}
	}
	return 0, false
}

// WaitOwnerWindow polls until pid or one of its ancestors owns a window. The
// window manager may need a moment to map a freshly started terminal, and a
// window that never shows up must not block the caller forever.
func WaitOwnerWindow(pid int, timeout time.Duration) (PidWindow, bool) {
	if pid <= 0 {
		return PidWindow{}, false
	}
	deadline := time.Now().Add(timeout)
	for {
		if w, ok := ownerWindow(pid); ok {
			return w, true
		}
		if !time.Now().Before(deadline) {
			return PidWindow{}, false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// PlaceOnDesktop moves a window to a desktop index.
func PlaceOnDesktop(xid string, desktop int) error {
	if xid == "" || desktop < 0 {
		return fmt.Errorf("place: invalid window %q or desktop %d", xid, desktop)
	}
	_, err := commandOut("wmctrl", "-i", "-r", xid, "-t", strconv.Itoa(desktop))
	return err
}

// PlaceSelfOnParentDesktop moves the window this process runs in onto the
// desktop of the terminal window holding parentPid, so a spawned agent window
// appears next to the terminal that spawned it rather than on whichever desktop
// happens to be focused. Returns the desktop it was placed on, or -1 when
// nothing was placed: headless, the caller has no terminal window in its parent
// chain, or this process's own window never showed up.
//
// Called from inside the new window, which is why the window to move is found
// via this process rather than guessed from the outside.
func PlaceSelfOnParentDesktop(parentPid int, timeout time.Duration) int {
	return PlaceSelfOnCallerDesktop("", parentPid, timeout)
}

// PlaceSelfOnCallerDesktop is PlaceSelfOnParentDesktop with an additional,
// preferred source for the caller's desktop: parentWindow, the terminal window
// registered for the caller's session (opencode TUI plugin -> register_window).
// That window is the only answer for sessions hosted by the shared
// `opencode serve`, whose pid chain holds no window at all. Its desktop is read
// at call time, so a window moved since registration is still found. When the
// window is gone the pid walk is tried as before.
func PlaceSelfOnCallerDesktop(parentWindow string, parentPid int, timeout time.Duration) int {
	if parentWindow == "" && parentPid <= 0 {
		return -1
	}
	if !haveDisplay() {
		return -1
	}
	desktop, ok := DesktopForWindow(parentWindow)
	if !ok && parentPid > 0 {
		desktop, ok = DesktopForProcess(parentPid)
	}
	if !ok {
		return -1
	}
	own, ok := WaitOwnerWindow(selfPID(), timeout)
	if !ok {
		return -1
	}
	if err := PlaceOnDesktop(own.XID, desktop); err != nil {
		return -1
	}
	return desktop
}

package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// windowsFile persists the session → terminal-window map (register_window) in
// the daemon data dir, so a daemon restart does not lose it.
const windowsFile = "windows.json"

// maxWindowEntries bounds the map. A new session beyond it evicts the least
// recently registered one, so the cap holds even without a window manager.
const maxWindowEntries = 256

// maxPid is Linux's pid_max ceiling; window_pid above it is malformed.
const maxPid = 1 << 22

// LiveWindow is one mapped window as the window manager reports it. Supplied
// by Config.LiveWindows (internal/terminals imports this package, so the
// daemon cannot call it directly).
type LiveWindow struct {
	XID     string
	Desktop int
	Pid     int // owner pid; X11 reuses window ids, the pid tells them apart
}

type windowRecord struct {
	Window   string `json:"window"`
	Terminal string `json:"terminal,omitempty"`
	Pid      int    `json:"pid,omitempty"`
	Seq      int64  `json:"seq,omitempty"`
}

type windowsSnapshot struct {
	Windows map[string]windowRecord `json:"windows"`
}

// canonXID returns the canonical form of an X11 id ("0x06800004" and
// "0x6800004" both become "0x6800004"), or "" when malformed.
func canonXID(s string) string {
	if !x11WindowID.MatchString(s) {
		return ""
	}
	n, err := strconv.ParseUint(s[2:], 16, 64)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("0x%x", n)
}

// setWindowLocked stores one registration. Caller holds windowMapMu.
func (h *Handler) setWindowLocked(sid string, rec windowRecord) {
	if h.windowPid == nil {
		h.windowPid = map[string]int{}
	}
	if h.windowSeq == nil {
		h.windowSeq = map[string]int64{}
	}
	h.windowMap[sid] = rec.Window
	if rec.Terminal != "" {
		h.terminalMap[sid] = rec.Terminal
	} else {
		delete(h.terminalMap, sid)
	}
	h.windowPid[sid] = rec.Pid
	h.windowSeq[sid] = rec.Seq
	if rec.Seq > h.windowNextSeq {
		h.windowNextSeq = rec.Seq
	}
}

// deleteWindowLocked removes one registration. Caller holds windowMapMu.
func (h *Handler) deleteWindowLocked(sid string) {
	delete(h.windowMap, sid)
	delete(h.terminalMap, sid)
	delete(h.windowPid, sid)
	delete(h.windowSeq, sid)
}

// evictOldestLocked drops the least recently registered session.
func (h *Handler) evictOldestLocked() {
	oldest, oldestSeq := "", int64(0)
	for sid := range h.windowMap {
		if s := h.windowSeq[sid]; oldest == "" || s < oldestSeq || (s == oldestSeq && sid < oldest) {
			oldest, oldestSeq = sid, s
		}
	}
	if oldest != "" {
		h.deleteWindowLocked(oldest)
	}
}

// RebuildWindowMap loads the persisted map at daemon start (file read only,
// no window-manager call). Invalid entries are skipped; when several sessions
// claim one window the most recent registration wins. A cleaned file is
// written back. Liveness is checked separately by PruneDeadWindows.
func (h *Handler) RebuildWindowMap() {
	if h.dataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(h.dataDir, windowsFile))
	if err != nil {
		return
	}
	var snap windowsSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		log.Printf("windows.json unreadable, ignored: %v", err)
		return
	}
	// Deterministic order: newest first, then by session id.
	sids := make([]string, 0, len(snap.Windows))
	for sid := range snap.Windows {
		sids = append(sids, sid)
	}
	sort.Slice(sids, func(i, j int) bool {
		a, b := snap.Windows[sids[i]], snap.Windows[sids[j]]
		if a.Seq != b.Seq {
			return a.Seq > b.Seq
		}
		return sids[i] < sids[j]
	})
	claimed := map[string]bool{}
	kept := 0
	h.windowMapMu.Lock()
	for _, sid := range sids {
		rec := snap.Windows[sid]
		canon := canonXID(rec.Window)
		if sid == "" || len(sid) > maxRegisterLen || len(rec.Terminal) > maxRegisterLen ||
			canon == "" || rec.Pid < 0 || rec.Pid > maxPid || rec.Seq < 0 || claimed[canon] || kept >= maxWindowEntries {
			continue
		}
		claimed[canon] = true
		h.setWindowLocked(sid, rec)
		kept++
	}
	h.windowMapMu.Unlock()
	if kept != len(snap.Windows) {
		h.saveWindowMap()
	}
}

// liveSnapshot asks the window manager once. known=false: can't tell.
// Never called while holding windowMapMu.
func (h *Handler) liveSnapshot() (map[string]LiveWindow, bool) {
	if h.liveWindows == nil {
		return nil, false
	}
	ws, err := h.liveWindows()
	if err != nil {
		return nil, false
	}
	live := make(map[string]LiveWindow, len(ws))
	for _, w := range ws {
		if c := canonXID(w.XID); c != "" {
			live[c] = w
		}
	}
	return live, true
}

// windowAlive reports whether a registered window still exists and still
// belongs to the client that registered it (pid 0 = not recorded).
func windowAlive(window string, pid int, live map[string]LiveWindow) (LiveWindow, bool) {
	lw, ok := live[canonXID(window)]
	if !ok {
		return LiveWindow{}, false
	}
	if pid > 0 && lw.Pid > 0 && lw.Pid != pid {
		return LiveWindow{}, false
	}
	return lw, true
}

// PruneDeadWindows drops registrations whose window is closed or whose id now
// belongs to another client. One window-manager call, outside the map lock;
// nothing is pruned when the window manager can't be asked. Registrations
// made while the window manager was being asked are newer than the snapshot
// and are left alone.
func (h *Handler) PruneDeadWindows() {
	h.windowMapMu.Lock()
	boundary := h.windowNextSeq
	h.windowMapMu.Unlock()
	live, known := h.liveSnapshot()
	if !known {
		return
	}
	n := 0
	h.windowMapMu.Lock()
	for sid, wid := range h.windowMap {
		if h.windowSeq[sid] > boundary {
			continue
		}
		if _, ok := windowAlive(wid, h.windowPid[sid], live); !ok {
			h.deleteWindowLocked(sid)
			n++
		}
	}
	h.windowMapMu.Unlock()
	if n > 0 {
		h.saveWindowMap()
	}
}

// windowPidOf returns the owner pid recorded for a session (0 = unknown).
func (h *Handler) windowPidOf(sessionID string) int {
	h.windowMapMu.Lock()
	defer h.windowMapMu.Unlock()
	return h.windowPid[sessionID]
}

// windowStatus returns a session's registered window and what the window
// manager says about it: known=false when it can't be asked (the
// registration is then assumed valid), alive=false when closed or reused.
func (h *Handler) windowStatus(sessionID string) (window string, lw LiveWindow, known, alive bool) {
	window = h.callerWindow(sessionID)
	if window == "" {
		return "", LiveWindow{}, false, false
	}
	pid := h.windowPidOf(sessionID)
	live, known := h.liveSnapshot()
	if !known {
		return window, LiveWindow{}, false, true
	}
	lw, alive = windowAlive(window, pid, live)
	return window, lw, true, alive
}

// windowInfo returns the session's window and, when the window manager can
// tell, the desktop it sits on right now. A closed or reused window yields "".
func (h *Handler) windowInfo(sessionID string) (window string, desktop int, haveDesktop bool) {
	window, lw, known, alive := h.windowStatus(sessionID)
	if !known {
		return window, 0, false
	}
	if !alive {
		return "", 0, false
	}
	return window, lw.Desktop, true
}

// saveWindowMap writes the current map atomically (temp file 0600, fsync,
// rename). windowSaveMu serialises writers so an older snapshot never
// overwrites a newer one.
func (h *Handler) saveWindowMap() {
	if h.dataDir == "" {
		return
	}
	h.windowSaveMu.Lock()
	defer h.windowSaveMu.Unlock()

	h.windowMapMu.Lock()
	snap := windowsSnapshot{Windows: make(map[string]windowRecord, len(h.windowMap))}
	for sid, wid := range h.windowMap {
		snap.Windows[sid] = windowRecord{Window: wid, Terminal: h.terminalMap[sid], Pid: h.windowPid[sid], Seq: h.windowSeq[sid]}
	}
	h.windowMapMu.Unlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		log.Printf("windows.json: %v", err)
		return
	}
	tmp, err := os.CreateTemp(h.dataDir, ".windows.json.*") // created 0600
	if err != nil {
		log.Printf("windows.json: %v", err)
		return
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		log.Printf("windows.json: %v", err)
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		log.Printf("windows.json: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		log.Printf("windows.json: %v", err)
		return
	}
	if err := os.Rename(tmp.Name(), filepath.Join(h.dataDir, windowsFile)); err != nil {
		log.Printf("windows.json: %v", err)
	}
}

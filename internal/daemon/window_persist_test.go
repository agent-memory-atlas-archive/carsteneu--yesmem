package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeDesk simulates the window manager: windows maps canonical xid → live
// window; unknown=true simulates "no display" (can't tell). during runs inside
// the window-manager call, to simulate registrations racing with it.
type fakeDesk struct {
	windows map[string]LiveWindow
	unknown bool
	calls   int
	during  func()
}

func (f *fakeDesk) live() ([]LiveWindow, error) {
	f.calls++
	if f.during != nil {
		f.during()
	}
	if f.unknown {
		return nil, errors.New("no display")
	}
	var res []LiveWindow
	for xid, w := range f.windows {
		w.XID = xid
		res = append(res, w)
	}
	return res, nil
}

func windowHandler(t *testing.T, dir string, desk *fakeDesk) *Handler {
	t.Helper()
	h, _ := mustHandler(t)
	h.dataDir = dir
	h.liveWindows = desk.live
	return h
}

func register(t *testing.T, h *Handler, sid, wid string, pid int) {
	t.Helper()
	p := map[string]any{"session_id": sid, "window_id": wid, "terminal": "ghostty"}
	if pid != 0 {
		p["window_pid"] = float64(pid)
	}
	if r := h.Handle(Request{Method: "register_window", Params: p}); r.Error != "" {
		t.Fatal(r.Error)
	}
}

func whoamiOf(t *testing.T, h *Handler, params map[string]any) map[string]any {
	t.Helper()
	r := h.Handle(Request{Method: "whoami", Params: params})
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	m := map[string]any{}
	if err := json.Unmarshal(r.Result, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The registration survives a daemon restart.
func TestWindowMap_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	desk := &fakeDesk{windows: map[string]LiveWindow{"0x6800004": {Desktop: 3, Pid: 100}, "0x5a00004": {Desktop: 1, Pid: 200}}}
	h1 := windowHandler(t, dir, desk)
	register(t, h1, "opencode:ses_a", "0x06800004", 100)
	register(t, h1, "opencode:ses_b", "0x05a00004", 200)

	h2 := windowHandler(t, dir, desk)
	h2.RebuildWindowMap()
	if got := h2.callerWindow("opencode:ses_a"); got != "0x06800004" {
		t.Fatalf("ses_a after restart = %q", got)
	}
	if got := h2.callerWindow("opencode:ses_b"); got != "0x05a00004" {
		t.Fatalf("ses_b after restart = %q", got)
	}
	h2.windowMapMu.Lock()
	term, pid := h2.terminalMap["opencode:ses_a"], h2.windowPid["opencode:ses_a"]
	h2.windowMapMu.Unlock()
	if term != "ghostty" || pid != 100 {
		t.Fatalf("terminal/pid after restart = %q/%d", term, pid)
	}
}

// Closed windows and windows whose id the X server handed to another client
// (owner pid differs) are pruned; with an unreachable window manager nothing
// is pruned. One wmctrl call per prune, made outside the map lock.
func TestWindowMap_PruneDeadWindows(t *testing.T) {
	dir := t.TempDir()
	setup := &fakeDesk{windows: map[string]LiveWindow{"0x6800004": {Pid: 100}, "0x5a00004": {Pid: 200}, "0x4400004": {Pid: 300}}}
	h1 := windowHandler(t, dir, setup)
	register(t, h1, "opencode:ses_live", "0x06800004", 100)
	register(t, h1, "opencode:ses_closed", "0x05a00004", 200)
	register(t, h1, "opencode:ses_reused", "0x04400004", 300)
	register(t, h1, "opencode:ses_nopid", "0x04c00004", 0)

	headless := windowHandler(t, dir, &fakeDesk{unknown: true})
	headless.RebuildWindowMap()
	headless.PruneDeadWindows()
	if n := len(headless.windowMap); n != 4 {
		t.Fatalf("unknown liveness pruned: %d left", n)
	}

	later := &fakeDesk{windows: map[string]LiveWindow{
		"0x6800004": {Desktop: 2, Pid: 100},
		"0x4400004": {Desktop: 2, Pid: 999}, // id reused by another client
		"0x4c00004": {Desktop: 5, Pid: 777}, // registered without pid: id check only
	}}
	h2 := windowHandler(t, dir, later)
	h2.RebuildWindowMap()
	h2.PruneDeadWindows()
	if later.calls != 1 {
		t.Fatalf("wmctrl calls = %d, want 1", later.calls)
	}
	for sid, want := range map[string]string{
		"opencode:ses_live": "0x06800004", "opencode:ses_nopid": "0x04c00004",
		"opencode:ses_closed": "", "opencode:ses_reused": "",
	} {
		h2.windowMapMu.Lock()
		got := h2.windowMap[sid]
		h2.windowMapMu.Unlock()
		if got != want {
			t.Errorf("%s = %q, want %q", sid, got, want)
		}
	}
	h3 := windowHandler(t, dir, &fakeDesk{unknown: true})
	h3.RebuildWindowMap()
	if n := len(h3.windowMap); n != 2 {
		t.Fatalf("prune not persisted: %d entries", n)
	}
}

func TestWindowMap_LoadValidatesAndDedupes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, windowsFile), []byte("{not json"), 0600)
	h := windowHandler(t, dir, &fakeDesk{unknown: true})
	h.RebuildWindowMap() // must not panic
	if n := len(h.windowMap); n != 0 {
		t.Fatalf("corrupt file loaded %d entries", n)
	}

	bad, _ := json.Marshal(map[string]any{"windows": map[string]any{
		"opencode:ok":    map[string]any{"window": "0x06800004", "terminal": "ghostty", "pid": 100, "seq": 2},
		"opencode:older": map[string]any{"window": "0x5a00004", "seq": 1},
		"opencode:newer": map[string]any{"window": "0x05a00004", "seq": 5}, // same window: newer wins
		"opencode:bad":   map[string]any{"window": "0x1; rm -rf ~"},
		"":               map[string]any{"window": "0x06800004"},
	}})
	os.WriteFile(filepath.Join(dir, windowsFile), bad, 0600)
	h2 := windowHandler(t, dir, &fakeDesk{unknown: true})
	h2.RebuildWindowMap()
	h2.windowMapMu.Lock()
	got := fmt.Sprint(h2.windowMap)
	h2.windowMapMu.Unlock()
	if got != "map[opencode:newer:0x05a00004 opencode:ok:0x06800004]" {
		t.Fatalf("loaded %s", got)
	}
	// The cleaned state is written back.
	data, _ := os.ReadFile(filepath.Join(dir, windowsFile))
	var snap windowsSnapshot
	json.Unmarshal(data, &snap)
	if len(snap.Windows) != 2 {
		t.Fatalf("not written back: %s", data)
	}
}

func TestWindowMap_FileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	h := windowHandler(t, dir, &fakeDesk{unknown: true})
	register(t, h, "opencode:ses_a", "0x06800004", 0)
	fi, err := os.Stat(filepath.Join(dir, windowsFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// The cap holds even when nothing can be pruned (headless daemon): the least
// recently registered session is evicted, without calling the window manager.
func TestWindowMap_CapEvictsOldest(t *testing.T) {
	desk := &fakeDesk{unknown: true}
	h := windowHandler(t, t.TempDir(), desk)
	for i := 0; i <= maxWindowEntries; i++ {
		register(t, h, fmt.Sprintf("opencode:ses_%d", i), fmt.Sprintf("0x%x", 0x1000004+i*0x100000), 0)
	}
	h.windowMapMu.Lock()
	n := len(h.windowMap)
	_, oldest := h.windowMap["opencode:ses_0"]
	_, newest := h.windowMap[fmt.Sprintf("opencode:ses_%d", maxWindowEntries)]
	h.windowMapMu.Unlock()
	if n != maxWindowEntries || oldest || !newest {
		t.Fatalf("n=%d oldest kept=%v newest kept=%v", n, oldest, newest)
	}
	if desk.calls != 0 {
		t.Fatalf("register called the window manager %d times", desk.calls)
	}
}

func TestRegisterWindow_RejectsBadPid(t *testing.T) {
	h := windowHandler(t, t.TempDir(), &fakeDesk{unknown: true})
	for _, pid := range []any{float64(-1), float64(0), float64(1.5), "100"} {
		r := h.Handle(Request{Method: "register_window", Params: map[string]any{"session_id": "opencode:a", "window_id": "0x06800004", "window_pid": pid}})
		if r.Error == "" {
			t.Errorf("window_pid %v accepted", pid)
		}
	}
}

// whoami tells an agent which window and desktop its session sits on; the
// desktop is read live, so a moved window reports its new desktop.
func TestWhoami_ReportsWindowAndDesktop(t *testing.T) {
	desk := &fakeDesk{windows: map[string]LiveWindow{"0x6800004": {Desktop: 3, Pid: 100}}}
	h := windowHandler(t, t.TempDir(), desk)
	register(t, h, "opencode:ses_a", "0x06800004", 100)

	m := whoamiOf(t, h, map[string]any{"session_id": "opencode:ses_a"})
	if m["window"] != "0x06800004" || m["desktop"] != float64(3) {
		t.Fatalf("whoami = %v", m)
	}
	desk.windows["0x6800004"] = LiveWindow{Desktop: 5, Pid: 100} // user moved the window
	if m := whoamiOf(t, h, map[string]any{"_session_id": "opencode:ses_a"}); m["desktop"] != float64(5) {
		t.Fatalf("desktop not read live: %v", m)
	}
}

func TestWhoami_NoGuessing(t *testing.T) {
	desk := &fakeDesk{windows: map[string]LiveWindow{"0x6800004": {Desktop: 3, Pid: 999}}}
	h := windowHandler(t, t.TempDir(), desk)
	register(t, h, "opencode:ses_a", "0x06800004", 100)

	// Window id reused by another client: no window, no desktop.
	if m := whoamiOf(t, h, map[string]any{"session_id": "opencode:ses_a"}); m["window"] != nil || m["desktop"] != nil {
		t.Fatalf("reused window reported: %v", m)
	}
	// Window manager unreachable: the registered window, but no desktop.
	desk.unknown = true
	if m := whoamiOf(t, h, map[string]any{"session_id": "opencode:ses_a"}); m["window"] != "0x06800004" || m["desktop"] != nil {
		t.Fatalf("unknown liveness: %v", m)
	}
	// Session only resolved via the global active-session fallback
	// (last writer wins across sessions): never report a window.
	desk.unknown = false
	desk.windows["0x6800004"] = LiveWindow{Desktop: 3, Pid: 100}
	h.activeSessionMu.Lock()
	h.activeSessionID = "opencode:ses_a"
	h.activeSessionMu.Unlock()
	if m := whoamiOf(t, h, map[string]any{}); m["window"] != nil || m["desktop"] != nil {
		t.Fatalf("fallback session reported a window: %v", m)
	}
}

// Spawn placement and save-terminals never get a window whose id now belongs
// to another client; an unreachable window manager keeps the registration.
func TestCallerWindowAndRegistrations_DropReusedWindows(t *testing.T) {
	desk := &fakeDesk{windows: map[string]LiveWindow{"0x6800004": {Pid: 100}, "0x5a00004": {Pid: 999}}}
	h := windowHandler(t, t.TempDir(), desk)
	register(t, h, "opencode:ok", "0x06800004", 100)
	register(t, h, "opencode:reused", "0x05a00004", 200)

	if got := h.callerWindowFromParams(map[string]any{"_session_id": "opencode:reused"}); got != "" {
		t.Fatalf("spawn got reused window %q", got)
	}
	if got := h.callerWindowFromParams(map[string]any{"_session_id": "opencode:ok"}); got != "0x06800004" {
		t.Fatalf("spawn lost live window: %q", got)
	}
	r := h.Handle(Request{Method: "list_registrations", Params: map[string]any{}})
	var reg struct{ Windows map[string]string }
	json.Unmarshal(r.Result, &reg)
	if _, ok := reg.Windows["opencode:reused"]; ok || reg.Windows["opencode:ok"] != "0x06800004" {
		t.Fatalf("list_registrations windows = %v", reg.Windows)
	}

	desk.unknown = true
	if got := h.callerWindowFromParams(map[string]any{"_session_id": "opencode:reused"}); got != "0x05a00004" {
		t.Fatalf("unknown liveness must keep the registration, got %q", got)
	}
}

// A window registered while the prune is asking the window manager is newer
// than the snapshot and must survive it (all TUIs re-register at daemon start).
func TestWindowMap_PruneKeepsRegistrationsNewerThanSnapshot(t *testing.T) {
	dir := t.TempDir()
	desk := &fakeDesk{windows: map[string]LiveWindow{"0x6800004": {Pid: 100}}}
	h := windowHandler(t, dir, desk)
	register(t, h, "opencode:ses_old", "0x05a00004", 200) // closed meanwhile
	desk.during = func() { desk.during = nil; register(t, h, "opencode:ses_new", "0x04400004", 300) }
	h.PruneDeadWindows()
	if got := h.callerWindow("opencode:ses_new"); got != "0x04400004" {
		t.Fatalf("fresh registration pruned: %q", got)
	}
	if got := h.callerWindow("opencode:ses_old"); got != "" {
		t.Fatalf("closed window kept: %q", got)
	}
}

// After a restart new registrations must rank newer than every loaded one,
// otherwise the cap would evict the newest session first.
func TestWindowMap_SeqContinuesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	desk := &fakeDesk{unknown: true}
	h1 := windowHandler(t, dir, desk)
	for i := 0; i < 5; i++ {
		register(t, h1, fmt.Sprintf("opencode:s%d", i), fmt.Sprintf("0x%x", 0x1000004+i*0x100000), 0)
	}
	h2 := windowHandler(t, dir, desk)
	h2.RebuildWindowMap()
	register(t, h2, "opencode:fresh", "0x7f00004", 0)
	h2.windowMapMu.Lock()
	fresh, loadedMax := h2.windowSeq["opencode:fresh"], h2.windowSeq["opencode:s4"]
	h2.windowMapMu.Unlock()
	if fresh <= loadedMax {
		t.Fatalf("seq after restart = %d, loaded max %d", fresh, loadedMax)
	}
}

func TestWindowMap_LoadRejectsNegativeSeq(t *testing.T) {
	dir := t.TempDir()
	bad, _ := json.Marshal(map[string]any{"windows": map[string]any{
		"opencode:neg": map[string]any{"window": "0x06800004", "seq": -5},
		"opencode:ok":  map[string]any{"window": "0x05a00004", "seq": 3},
	}})
	os.WriteFile(filepath.Join(dir, windowsFile), bad, 0600)
	h := windowHandler(t, dir, &fakeDesk{unknown: true})
	h.RebuildWindowMap()
	if h.callerWindow("opencode:neg") != "" || h.callerWindow("opencode:ok") == "" {
		t.Fatalf("loaded %v", h.windowMap)
	}
}

// Claude sessions never register a window: whoami must not touch the window
// manager for them.
func TestWhoami_UnregisteredSessionNeverAsksWindowManager(t *testing.T) {
	desk := &fakeDesk{windows: map[string]LiveWindow{}}
	h := windowHandler(t, t.TempDir(), desk)
	m := whoamiOf(t, h, map[string]any{"session_id": "claude-session-1"})
	if m["window"] != nil || desk.calls != 0 {
		t.Fatalf("whoami = %v, window-manager calls = %d", m, desk.calls)
	}
}

// Spawn placement asks the window manager at most once per spawn.
func TestCallerWindowFromParams_OneSnapshot(t *testing.T) {
	desk := &fakeDesk{windows: map[string]LiveWindow{"0x6800004": {Pid: 100}}}
	h := windowHandler(t, t.TempDir(), desk)
	register(t, h, "opencode:agent", "0x05a00004", 200) // closed
	register(t, h, "opencode:main", "0x06800004", 100)
	desk.calls = 0
	got := h.callerWindowFromParams(map[string]any{"_session_id": "opencode:agent", "caller_session": "opencode:main"})
	if got != "0x06800004" || desk.calls != 1 {
		t.Fatalf("window %q after %d window-manager calls; want 0x06800004 after 1", got, desk.calls)
	}
}

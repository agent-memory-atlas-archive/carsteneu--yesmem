package terminals

import "testing"

// LiveWindows reads every mapped window with exactly one `wmctrl -lp` call.
// err != nil means "can't tell" (no display, wmctrl failed): callers must not
// treat a window as closed then.
func TestLiveWindows(t *testing.T) {
	st := &x11Stub{display: true, windows: callerWindow + "\n" + agentWindow}
	stubX11(t, st)

	ws, err := LiveWindows()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %v", len(ws), ws)
	}
	w := ws[0]
	if w.XID != "0x4c00004" || w.Workspace != 11 || w.Pid != 1626101 {
		t.Fatalf("first window = %+v", w)
	}
	if st.calls != 1 {
		t.Fatalf("wmctrl calls = %d, want 1", st.calls)
	}
}

func TestLiveWindows_NoDisplayIsUnknown(t *testing.T) {
	stubX11(t, &x11Stub{display: false, lpFail: true, windows: callerWindow})

	if ws, err := LiveWindows(); err == nil || ws != nil {
		t.Fatalf("headless: %v, %v; want nil + error", ws, err)
	}
}

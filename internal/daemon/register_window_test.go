package daemon

import (
	"strings"
	"testing"
)

// A window shows exactly one session. When a TUI switches from session A to
// B, A must no longer claim the window, otherwise save-terminals picks A or B
// at random (map iteration) and terminals.json flips between saves.
func TestHandleRegisterWindow_OneSessionPerWindow(t *testing.T) {
	h, _ := mustHandler(t)
	reg := func(sid, wid string) {
		t.Helper()
		if r := h.Handle(Request{Method: "register_window", Params: map[string]any{"session_id": sid, "window_id": wid, "terminal": "ghostty"}}); r.Error != "" {
			t.Fatal(r.Error)
		}
	}
	reg("opencode:ses_a", "0x06800004")
	reg("opencode:ses_other", "0x05a00004")
	reg("opencode:ses_b", "0x6800004") // same window (unpadded form) now shows ses_b

	h.windowMapMu.Lock()
	defer h.windowMapMu.Unlock()
	if _, ok := h.windowMap["opencode:ses_a"]; ok {
		t.Errorf("ses_a still claims the window: %v", h.windowMap)
	}
	if _, ok := h.terminalMap["opencode:ses_a"]; ok {
		t.Errorf("ses_a terminal entry left behind: %v", h.terminalMap)
	}
	if h.windowMap["opencode:ses_b"] != "0x6800004" || h.windowMap["opencode:ses_other"] != "0x05a00004" {
		t.Errorf("unexpected map: %v", h.windowMap)
	}
}

// register_window accepts input from any same-uid socket client: validate on
// write so malformed ids and oversized keys never enter windowMap.
func TestHandleRegisterWindow_RejectsMalformedInput(t *testing.T) {
	h, _ := mustHandler(t)
	long := strings.Repeat("s", 300)
	for _, p := range []map[string]any{
		{"session_id": "opencode:ses_a", "window_id": "0x04c00004; rm -rf ~"},
		{"session_id": "opencode:ses_a", "window_id": "-t"},
		{"session_id": "opencode:ses_a", "window_id": "0x1234567890abcdef0"},
		{"session_id": long, "window_id": "0x04c00004"},
		{"session_id": "opencode:ses_a", "window_id": "0x04c00004", "terminal": long},
	} {
		if r := h.Handle(Request{Method: "register_window", Params: p}); r.Error == "" {
			t.Errorf("accepted %v", p)
		}
	}
	h.windowMapMu.Lock()
	n := len(h.windowMap)
	h.windowMapMu.Unlock()
	if n != 0 {
		t.Fatalf("windowMap polluted: %d entries", n)
	}
}

package daemon

import (
	"fmt"
	"reflect"
	"testing"
)

func TestCallerPidParam(t *testing.T) {
	if got := callerPidParam(map[string]any{"_caller_pid": float64(4711)}); got != 4711 {
		t.Fatalf("callerPidParam = %d, want 4711", got)
	}
	for _, p := range []map[string]any{
		{},
		{"_caller_pid": "4711"}, // MCP injects a JSON number, never a string
		{"_caller_pid": float64(0)},
		{"_caller_pid": float64(-1)},
	} {
		if got := callerPidParam(p); got != 0 {
			t.Errorf("callerPidParam(%v) = %d, want 0", p, got)
		}
	}
}

func TestShouldPlaceAgentWindow(t *testing.T) {
	cases := []struct {
		name         string
		terminal     string
		parentPid    int
		parentWindow string
		want         bool
	}{
		{"ghostty with a caller pid", "ghostty", 900, "", true},
		{"tmux opens no window of its own", "tmux", 900, "", false},
		{"tmux ignores a caller window too", "tmux", 0, "0x04c00004", false},
		{"no caller pid, nothing to inherit", "ghostty", 0, "", false},
		{"unclassified terminal is still placed", "unknown", 900, "", true},
		{"caller window alone (shared opencode serve)", "ghostty", 0, "0x04c00004", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldPlaceAgentWindow(c.terminal, c.parentPid, c.parentWindow); got != c.want {
				t.Fatalf("shouldPlaceAgentWindow(%q, %d, %q) = %v, want %v", c.terminal, c.parentPid, c.parentWindow, got, c.want)
			}
		})
	}
}

func TestBuildAgentTTYArgs(t *testing.T) {
	cases := []struct {
		name     string
		terminal string
		pid      int
		window   string
		want     []string
	}{
		{"no placement source", "ghostty", 0, "", []string{"agent-tty", "--sock", "/d/a.sock"}},
		{"pid only (Claude Code, standalone opencode)", "ghostty", 900, "", []string{"agent-tty", "--sock", "/d/a.sock", "--parent-pid", "900"}},
		{"window only", "ghostty", 0, "0x04c00004", []string{"agent-tty", "--sock", "/d/a.sock", "--parent-window", "0x04c00004"}},
		{"both", "ghostty", 1706879, "0x04c00004", []string{"agent-tty", "--sock", "/d/a.sock", "--parent-pid", "1706879", "--parent-window", "0x04c00004"}},
		{"tmux never placed", "tmux", 900, "0x04c00004", []string{"agent-tty", "--sock", "/d/a.sock"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildAgentTTYArgs("/d/a.sock", c.terminal, c.pid, c.window)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("buildAgentTTYArgs = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCallerWindow(t *testing.T) {
	h := &Handler{windowMap: map[string]string{
		"opencode:ses_a": "0x04c00004",
		"opencode:ses_b": "0x05a00004",
	}, terminalMap: map[string]string{}}

	if got := h.callerWindow("opencode:ses_a"); got != "0x04c00004" {
		t.Fatalf("callerWindow(ses_a) = %q", got)
	}
	// Two sessions on two desktops resolve independently.
	if got := h.callerWindow("opencode:ses_b"); got != "0x05a00004" {
		t.Fatalf("callerWindow(ses_b) = %q", got)
	}
	for _, sid := range []string{"", "opencode:unknown", "ses_a"} {
		if got := h.callerWindow(sid); got != "" {
			t.Errorf("callerWindow(%q) = %q, want empty", sid, got)
		}
	}
}

// register_window accepts any string from the socket; only a well-formed X11
// window id may travel on into the agent-tty argv.
func TestCallerWindow_RejectsMalformedIDs(t *testing.T) {
	bad := []string{"0x04c00004; rm -rf ~", "-t", "0x", "0xZZ", "123456", "0x" + "1234567890abcdef0"}
	h := &Handler{windowMap: map[string]string{}, terminalMap: map[string]string{}}
	for i, v := range bad {
		sid := fmt.Sprintf("opencode:bad%d", i)
		h.windowMap[sid] = v
		if got := h.callerWindow(sid); got != "" {
			t.Errorf("callerWindow for stored %q = %q, want empty", v, got)
		}
	}
	h.windowMap["opencode:ok"] = "0X4C00004"
	if got := h.callerWindow("opencode:ok"); got != "0X4C00004" {
		t.Errorf("valid upper-case id rejected: %q", got)
	}
}

// Only authoritative session ids may pick a window: the global
// active_session_opencode fallback of resolveSessionID is last-writer-wins
// across sessions and would place the agent on another session's desktop.
// The per-call _session_id (where the caller really is) wins; caller_session
// (the reply target) is the fallback when the caller itself has no window,
// e.g. an orchestrator agent behind the daemon's PTY bridge.
func TestCallerWindowFromParams(t *testing.T) {
	h := &Handler{windowMap: map[string]string{
		"opencode:ses_main": "0x04c00004",
		"opencode:ses_b":    "0x05a00004",
	}, terminalMap: map[string]string{}}
	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"per-call session with a window wins", map[string]any{"caller_session": "opencode:ses_main", "_session_id": "opencode:ses_b"}, "0x05a00004"},
		{"caller without window falls back to caller_session's window", map[string]any{"caller_session": "opencode:ses_main", "_session_id": "opencode:agent_x"}, "0x04c00004"},
		{"only caller_session", map[string]any{"caller_session": "opencode:ses_main"}, "0x04c00004"},
		{"only per-call session", map[string]any{"_session_id": "opencode:ses_b"}, "0x05a00004"},
		{"nothing authoritative", map[string]any{"_caller_pid": float64(900), "_source_agent": "opencode"}, ""},
		{"no window anywhere", map[string]any{"caller_session": "opencode:x", "_session_id": "opencode:y"}, ""},
		{"wrong types ignored", map[string]any{"caller_session": 42, "_session_id": true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := h.callerWindowFromParams(c.params); got != c.want {
				t.Fatalf("callerWindowFromParams = %q, want %q", got, c.want)
			}
		})
	}
}

// The full producer→consumer path inside the daemon: what the opencode TUI
// plugin registers is what spawn_agent hands to agent-tty.
func TestRegisterWindowFeedsCallerWindow(t *testing.T) {
	h := &Handler{windowMap: map[string]string{}, terminalMap: map[string]string{}}
	resp := h.handleRegisterWindow(map[string]any{
		"session_id": "opencode:ses_x",
		"window_id":  "0x06800004",
		"terminal":   "ghostty",
	})
	if resp.Error != "" {
		t.Fatalf("register_window: %s", resp.Error)
	}
	if got := h.callerWindow("opencode:ses_x"); got != "0x06800004" {
		t.Fatalf("callerWindow after register = %q", got)
	}
	// Re-registering (session shown in another window now) replaces it.
	h.handleRegisterWindow(map[string]any{"session_id": "opencode:ses_x", "window_id": "0x05a00004"})
	if got := h.callerWindow("opencode:ses_x"); got != "0x05a00004" {
		t.Fatalf("callerWindow after re-register = %q", got)
	}
}

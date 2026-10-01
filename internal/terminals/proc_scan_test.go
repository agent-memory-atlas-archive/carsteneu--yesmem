package terminals

import (
	"runtime"
	"os"
	"testing"
)

func TestSessionFromCmdline(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"dash-s", []string{"/usr/bin/opencode", "-s", "ses_abc123"}, "ses_abc123"},
		{"equals", []string{"opencode", "--session=ses_xyz789"}, "ses_xyz789"},
		{"no-session", []string{"opencode", "run", "--x"}, ""},
		{"tui-server", []string{"opencode", "__opencode_tui_server__"}, ""},
		{"non-ses-id", []string{"opencode", "-s", "deadbeef-uuid"}, ""},
		{"trailing-flag", []string{"opencode", "-s"}, ""},
		{"empty", nil, ""},
		{"with-session-after-other-args", []string{"opencode", "--log", "debug", "-s", "ses_ok1"}, "ses_ok1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sessionFromCmdline(c.args); got != c.want {
				t.Fatalf("sessionFromCmdline(%v) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}

func TestPidCmdline(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("/proc-based cmdline lookup is Linux-only (GOOS=%s)", runtime.GOOS)
	}
	// Unser eigener Testprozess hat eine cmdline. NUL-Split muss sie liefern.
	got := pidCmdline(os.Getpid())
	if len(got) == 0 || got[0] == "" {
		t.Fatalf("pidCmdline(self) empty: %v", got)
	}
}

func TestUuidFromRolloutPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{
			"live-tui",
			"/home/testuser/.codex/sessions/2026/09/02/rollout-2026-09-02T22-59-57-01a063eb-df24-7b40-a5ea-46f99b8a4a3a.jsonl",
			"01a063eb-df24-7b40-a5ea-46f99b8a4a3a",
		},
		{"exec-session", "/home/testuser/.codex/sessions/2026/09/02/rollout-2026-09-02T22-58-00-01a063ea-16b9-72a2-86ed-277c2c2d0e04.jsonl",
			"01a063ea-16b9-72a2-86ed-277c2c2d0e04"},
		{"no-jsonl", "/home/testuser/.codex/sessions/2026/09/02/rollout-2026-09-02T22-59-57-01a063eb-df24-7b40-a5ea-46f99b8a4a3a",
			"01a063eb-df24-7b40-a5ea-46f99b8a4a3a"},
		{"short", "/home/testuser/.codex/sessions/rollout-nouuid.jsonl", ""},
		{"non-hex", "/home/testuser/.codex/sessions/2026/09/02/rollout-2026-09-02T22-59-57-01zzzzbb-df24-7b40-a5ea-46f99b8a4a3a.jsonl", ""},
		{"foreign-file", "/home/testuser/.codex/sessions/2026/09/02/other.txt", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := uuidFromRolloutPath(c.path); got != c.want {
				t.Fatalf("uuidFromRolloutPath(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestClassifyEmulatorGnomeTerminal(t *testing.T) {
	if got := ClassifyEmulator([]string{"gnome-terminal-server", "Gnome-terminal"}); got != "gnome-terminal" {
		t.Fatalf("ClassifyEmulator(gnome-terminal) = %q, want %q", got, "gnome-terminal")
	}
}

func TestResumeCommandCodex(t *testing.T) {
	if got := ResumeCommand("codex", "01a063eb-df24-7b40-a5ea-46f99b8a4a3a"); got != "codex resume 01a063eb-df24-7b40-a5ea-46f99b8a4a3a" {
		t.Fatalf("codex resume wrong: %q", got)
	}
	if got := ResumeCommand("codex", ""); got != "codex" {
		t.Fatalf("codex fresh wrong: %q", got)
	}
	if got := ResumeCommand("codex", "01a063eb"); got != "codex resume 01a063eb" {
		t.Fatalf("codex resume short-id should pass through: %q", got)
	}
}

func TestDedupeSessionEntries(t *testing.T) {
	s := Empty()
	s.Windows = []Window{
		{Kind: "opencode", SessionID: "ses_a"},
		{Kind: "opencode", SessionID: "ses_a", XID: "0x100", Emulator: "ghostty"},
		{Kind: "codex", SessionID: "uuid-b", XID: "0x200"},
		{Kind: "shell"},
		{Kind: "claude", SessionID: "cc-1", XID: "0x300"},
	}
	dedupeSessionEntries(s)
	if got := len(s.Windows); got != 4 {
		t.Fatalf("want 4 windows after dedupe, got %d: %+v", got, s.Windows)
	}
	var open Window
	n := 0
	for _, w := range s.Windows {
		if w.SessionID == "ses_a" {
			n++
			open = w
		}
	}
	if n != 1 {
		t.Fatalf("ses_a duplicated: %d entries", n)
	}
	if open.XID != "0x100" || open.Emulator != "ghostty" {
		t.Fatalf("XID-backed entry must win: %+v", open)
	}
}

func TestPruneDeadSessions(t *testing.T) {
	s := Empty()
	s.Windows = []Window{
		{Kind: "opencode", SessionID: "ses_dead"},
		{Kind: "opencode", SessionID: "ses_live"},
		{Kind: "codex", SessionID: "uuid-c", XID: "0x200"},
		{Kind: "claude", SessionID: "cc-1"},
	}
	pruneDeadSessions(s, map[string]bool{"ses_live": true, "uuid-c": true})
	for _, w := range s.Windows {
		if w.SessionID == "ses_dead" {
			t.Fatalf("dead tombstone not pruned: %+v", s.Windows)
		}
	}
	if got := len(s.Windows); got != 3 {
		t.Fatalf("want 3 remaining windows, got %d: %+v", got, s.Windows)
	}
}

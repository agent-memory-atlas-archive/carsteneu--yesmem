package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The title reaches buildShellCmd from caller-supplied data (the agent section),
// and it is embedded in a shell command. It must not be able to end its quoting
// and run something else.
func TestBuildShellCmd_TitleCannotInjectCommands(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "injected")
	title := "x'; touch " + marker + "; echo '"

	cmd := buildShellCmd("true", nil, title)

	out, err := exec.Command("bash", "-c", cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -c failed: %v (%q)", err, out)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("title injected a command — %s was created; generated: %s", marker, cmd)
	}
	// The title must still reach the terminal unchanged.
	if want := "]0;" + title; !strings.Contains(string(out), want) {
		t.Fatalf("title not emitted literally:\n got %q\nwant it to contain %q\ncommand: %s", out, want, cmd)
	}
}

// A harmless title must keep producing exactly the old escape sequence, so the
// fix does not silently change the window title format.
func TestBuildShellCmd_PlainTitleUnchanged(t *testing.T) {
	cmd := buildShellCmd("true", nil, "yesmem-task-a #7")
	if !strings.Contains(cmd, `echo -ne '\033]0;yesmem-task-a #7\007';`) {
		t.Fatalf("plain title changed shape: %s", cmd)
	}
}

// TestBuildShellCmd_TitleBackslashEscapesStayLiteral: the title is emitted with
// `echo -e`, so a title carrying the LITERAL text "\033]2;" would be turned into a
// second escape sequence by echo itself — a window-title or hyperlink injection
// without a single control character in the value (YM-416, cold review finding).
// The backslash has to reach the terminal as a backslash.
func TestBuildShellCmd_TitleBackslashEscapesStayLiteral(t *testing.T) {
	title := `yesmem-x\033]2;FORGED\007 #7`

	out, err := exec.Command("bash", "-c", buildShellCmd("true", nil, title)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -c failed: %v (%q)", err, out)
	}
	if strings.Contains(string(out), "\x1b]2;") {
		t.Fatalf("the title produced a second escape sequence: %q", out)
	}
	if !strings.Contains(string(out), title) {
		t.Fatalf("title not emitted literally:\n got %q\nwant it to contain %q", out, title)
	}
	if n := strings.Count(string(out), "\x1b]"); n != 1 {
		t.Fatalf("expected exactly one OSC introducer, got %d: %q", n, out)
	}
}

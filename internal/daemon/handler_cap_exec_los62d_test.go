package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-448 [SEC-C04] (Los 62d): the mask path a caller hands in reaches the daemon
// log through PrepareErr, and newSandbox printed that error verbatim. A path with
// an embedded line break therefore produced an extra journal line of its own —
// the same class as YM-440 and YM-447. The refusal has to stay one line, with the
// value quoted and escaped.
func TestLos62dCallerMaskPathCannotForgeALogLine(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)

	// A mask path that cannot be materialised (a regular file where a directory
	// would have to be) and that carries a line break plus a journal-style prefix.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	// The slash date is the journal's own prefix (log.LstdFlags), so the forged line
	// would read as a genuine entry.
	forged := "2026/10/23 12:00:00 forged journal line"
	unmaterialisable := filepath.Join(blocker, "child", "mask\n"+forged+".json")

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	sb := h.newSandbox(SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		WorkDir:             t.TempDir(),
		MaskedPaths:         []string{unmaterialisable},
	})

	out := buf.String()
	if sb.cfg.PrepareErr == nil {
		t.Fatalf("an unmaterialisable mask path prepared without a refusal; log: %q", out)
	}

	var refusal string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "refusing sandboxed runs") {
			refusal = line
		}
	}
	if refusal == "" {
		t.Fatalf("no refusal line in the log: %q", out)
	}
	// The value must appear quoted and escaped in the refusal line itself.
	if !strings.Contains(refusal, `\n`+forged) {
		t.Errorf("the refusal line does not carry the path escaped and on one line: %q", refusal)
	}
	// And nothing in the log may start on the injected prefix.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, forged) {
			t.Errorf("the caller path forged a journal line: %q (log: %q)", line, out)
		}
	}
}

// The second form of the same defect needs no caller input: two prepare causes are
// joined, and errors.Join always puts a line break between them. The refusal has to
// stay one line here too.
func TestLos62dJoinedPrepareCausesStayOnOneLine(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	sb := h.newSandbox(SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		WorkDir:             filepath.Join(t.TempDir(), "gone"),
		MaskedPaths:         []string{filepath.Join(blocker, "child", "mask.json")},
	})

	out := buf.String()
	if sb.cfg.PrepareErr == nil {
		t.Fatalf("two prepare causes prepared without a refusal; log: %q", out)
	}
	// Every physical line has to be a journal record of its own. A value that
	// slipped in raw starts a line that does not begin with the log prefix.
	refusals := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if len(line) < 11 || line[4] != '/' || line[7] != '/' || line[10] != ' ' {
			t.Errorf("a value forged a journal line: %q (log: %q)", line, out)
		}
		if strings.Contains(line, "refusing sandboxed runs") {
			refusals++
			if !strings.Contains(line, `\n`) {
				t.Errorf("the joined causes are not escaped on the refusal line: %q", line)
			}
		}
	}
	if refusals != 1 {
		t.Errorf("the log carries %d refusal lines, want one: %q", refusals, out)
	}
}

package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-441 [OPS-109] Teil 1 (Los 61c): newSandbox kann zwei Ursachen gleichzeitig
// finden — eine Maske, die sich nicht anlegen laesst, und eine benoetigte
// Mount-Quelle, die unter einer Maske verschwindet. PrepareErr traegt nur eine
// davon, und die Maske gewinnt. Der Nachbarzweig fuer den WorkDir-Fall (der
// workDirCoversHome-Zweig in newSandbox) loggt die zweite Ursache an derselben
// Stelle; der Mount-Fall verwarf sie. Folge: im Daemon-Log stand nur das
// allgemeine Fehlbild, nicht der Grund, an dem die Diagnose genau dieser Klasse
// haengt (YM-428/YM-437).
//
// Die trennende Zusicherung ist der Satz der neuen Zeile; die Zeilen davor
// ("dropping mount ...", der Socket-Pfad) stammen aus dropMountsInside und stehen
// schon vor dem Fix im Log. Sie bleiben als Kontext der Meldung stehen. Der Satz
// ist bewusst nicht der Text von lostErr: der traegt errMaskPrepare und wuerde eine
// fehlgeschlagene Maske behaupten, obwohl hier eine Maske gegriffen hat.
func TestLos61cLostRequiredMountIsLoggedBehindTheMaskCause(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)

	// A mask path whose parent is a regular file: materialiseMaskPaths cannot lay
	// the placeholder down, so maskErr is set.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	unmaterialisable := filepath.Join(blocker, "child")

	// A second mask over the runtime dir: it takes the job socket with it, and that
	// socket is one of the mounts a sandboxed run cannot work without.
	runtimeDir := RuntimeDir(h.dataDir)
	sock := JobSocketPath(h.dataDir)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	sb := h.newSandbox(SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		WorkDir:             t.TempDir(),
		MaskedPaths:         []string{unmaterialisable, runtimeDir + string(filepath.Separator)},
	})

	out := buf.String()
	if sb.cfg.PrepareErr == nil {
		t.Fatalf("two prepare causes prepared without a refusal; log: %q", out)
	}
	// Precedence stays as it was: the mask cause is the one on record.
	if !strings.Contains(sb.cfg.PrepareErr.Error(), "mask placeholder") {
		t.Errorf("PrepareErr %q no longer carries the mask cause", sb.cfg.PrepareErr)
	}
	// The dropped socket is the second cause, and it has to reach the log.
	if !strings.Contains(out, "dropping mount") || !strings.Contains(out, sock) {
		t.Errorf("the log does not name the dropped mount %q: %q", sock, out)
	}
	if !strings.Contains(out, "a mask took a mount the run needs") {
		t.Errorf("the log lost the mount cause behind the mask cause: %q", out)
	}
	// The line must not read as a mask failure: that is the cause it stands next to.
	// Printing lostErr verbatim would prefix it with the errMaskPrepare sentence, so
	// the check runs on the line itself rather than on the whole buffer.
	var causeLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "a mask took a mount the run needs") {
			causeLine = line
		}
	}
	if causeLine == "" {
		t.Fatalf("no line names the mount cause: %q", out)
	}
	if strings.Contains(causeLine, "could not be applied") || strings.Contains(causeLine, "mask placeholder") {
		t.Errorf("the mount line is worded as a mask failure: %q", causeLine)
	}
}

// The other direction: without a mask cause the mount cause is the refusal and
// reaches the log through the single message. It must be logged once, not twice —
// a fix that logs the mount cause unconditionally would turn every ordinary
// refused run into two lines about the same thing.
func TestLos61cLostRequiredMountIsLoggedOnceWithoutAMaskCause(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)

	runtimeDir := RuntimeDir(h.dataDir)
	sock := JobSocketPath(h.dataDir)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	sb := h.newSandbox(SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		WorkDir:             t.TempDir(),
		MaskedPaths:         []string{runtimeDir + string(filepath.Separator)},
	})

	out := buf.String()
	if sb.cfg.PrepareErr == nil {
		t.Fatalf("a lost job socket prepared without a refusal; log: %q", out)
	}
	if !strings.Contains(sb.cfg.PrepareErr.Error(), "a mount the run needs") {
		t.Errorf("PrepareErr %q does not carry the mount cause", sb.cfg.PrepareErr)
	}
	if n := strings.Count(out, "a mount the run needs"); n != 1 {
		t.Errorf("the mount cause appears %d times in the log, want exactly once: %q", n, out)
	}
	if !strings.Contains(out, sock) {
		t.Errorf("the log does not name the dropped mount %q: %q", sock, out)
	}
}

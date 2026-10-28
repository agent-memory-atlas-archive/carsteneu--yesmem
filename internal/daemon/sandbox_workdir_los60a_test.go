package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-437 (OPS-107): ein WorkDir, den es nicht mehr gibt, faellt in runProcess auf
// den cwd des Daemons zurueck, und der Kommentar an claudeConfigFlags verspricht
// fuer diesen Fall den Masken-Zweig, den der Code nicht nimmt. Ein leerer WorkDir
// erbt denselben cwd mit einer WARNING. Beides ist die naechste Form des Problems,
// das YM-428 fuer den deckenden WorkDir geschlossen hat: die Entscheidung gehoert in
// den Prepare-Schritt, nicht in den Start.
func TestLos60aVanishedWorkDirIsRefused(t *testing.T) {
	isolateHome(t)
	los34cPinAiJail(t, 0)
	h, _ := mustHandler(t)
	gone := filepath.Join(t.TempDir(), "gone")
	if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe work dir %q exists, the test would not prove the vanished case: %v", gone, err)
	}

	sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, WorkDir: gone})
	if sb.cfg.PrepareErr == nil {
		t.Fatalf("a vanished work dir (%s) prepared without a refusal, the run would start in the daemon's cwd", gone)
	}
	if !errors.Is(sb.cfg.PrepareErr, errNoWorkDir) {
		t.Errorf("PrepareErr %q does not carry errNoWorkDir", sb.cfg.PrepareErr)
	}

	out, code, err := sb.Run("printf LOS60A-RAN", 10, ProfileStandard)
	if err == nil {
		t.Fatalf("the standard run started (exit=%d out=%q), it would run in the daemon's own directory", code, out)
	}
	if !errors.Is(err, errNoWorkDir) {
		t.Errorf("run error %q does not carry errNoWorkDir", err)
	}
	// The run never started, so the auto-correct pass must not read this as a
	// script failure (bash_error_handler.go, handler_scheduler.go).
	if !runNeverStarted(err) {
		t.Errorf("run error %q is not classified as a run that never started", err)
	}
	if code != -1 || out != "" {
		t.Errorf("a refused run reported exit=%d out=%q, want -1 and no output", code, out)
	}

	// The refusal has to be actionable: the work dir is the caller's own input
	// (Befund 5, YM-334), so the text names it instead of blaming a mask.
	text := maskRefusalForCaller(err)
	if !strings.Contains(text, gone) {
		t.Errorf("caller text %q does not name the work dir %q", text, gone)
	}
	if strings.Contains(text, "mask") {
		t.Errorf("caller text %q blames a mask, the operator would look in the mask list instead of the work dir", text)
	}
	if strings.ContainsAny(text, "\r\n") {
		t.Errorf("caller text carries a raw line break or carriage return: %q", text)
	}

	// sandbox: none binds no cwd at all, so the same missing work dir has to stay a
	// legitimate run there.
	out, code, err = sb.Run("printf LOS60A-NONE", 10, ProfileNone)
	if err != nil || code != 0 {
		t.Fatalf("a none run must stay allowed without a work dir: exit=%d err=%v out=%q", code, err, out)
	}
	if strings.TrimSpace(out) != "LOS60A-NONE" {
		t.Errorf("none run output = %q, want the probe", out)
	}
}

// The joined causes have to be recorded by newSandbox itself, not only by the gate: a
// nil-check in the style of the covering-home block would drop the work-dir cause
// behind a mask cause recorded earlier, and a strict run — which renders no masks —
// would start in the daemon's cwd. A missing home makes the mask defaults fail and
// puts a real mask cause on record (TestLos34gRefusesARunWithoutAHomeDirectory).
func TestLos60aVanishedWorkDirBesideAMaskCause(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	unsetEnvForTest(t, "HOME")
	gone := filepath.Join(t.TempDir(), "gone")

	sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, WorkDir: gone})
	if sb.cfg.PrepareErr == nil {
		t.Fatalf("a vanished work dir beside a mask cause prepared without a refusal")
	}
	if !strings.Contains(sb.cfg.PrepareErr.Error(), "need a home directory") {
		t.Fatalf("the mask cause is not on record, the test would not prove the join: %v", sb.cfg.PrepareErr)
	}
	if !errors.Is(sb.cfg.PrepareErr, errNoWorkDir) {
		t.Fatalf("the mask cause swallowed the work-dir cause: %v", sb.cfg.PrepareErr)
	}

	// Both binding profiles refuse on the work-dir cause, and the caller text stays
	// one line even though PrepareErr carries two causes.
	for _, profile := range []SandboxProfile{ProfileStandard, ProfileStrict} {
		err := sb.maskFailure(profile)
		if !errors.Is(err, errNoWorkDir) {
			t.Errorf("profile %s must be refused on the work-dir cause, got %v", profile, err)
		}
		if err != nil && strings.ContainsAny(maskRefusalForCaller(err), "\r\n") {
			t.Errorf("profile %s rendered a caller text with a line break: %q", profile, maskRefusalForCaller(err))
		}
	}
}

// The second half of the same finding: WorkDir == "" inherited the daemon's cwd with
// only a WARNING behind it. newSandbox sees that case only when the runtime dir
// cannot provide a default either (YM-286), so the gate is checked on its own: the
// refusal must not depend on which of the two causes came first, and a mask cause
// recorded earlier must not hide it (a strict run renders no masks, so a lost work
// dir cause would let it start in the daemon's cwd).
func TestLos60aMissingWorkDirIsRefused(t *testing.T) {
	isolateHome(t)
	los34cPinAiJail(t, 0)

	causes := map[string]error{
		"work dir cause alone": noWorkDirErr(""),
		"beside a mask cause":  errors.Join(errors.New("mask placeholder /nowhere: denied"), noWorkDirErr("")),
	}
	for name, cause := range causes {
		t.Run(name, func(t *testing.T) {
			sb := NewSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, PrepareErr: cause})

			if err := sb.maskFailure(ProfileNone); err != nil {
				t.Errorf("the none profile binds nothing, it must not be refused: %v", err)
			}
			for _, profile := range []SandboxProfile{ProfileStandard, ProfileStrict} {
				err := sb.maskFailure(profile)
				if !errors.Is(err, errNoWorkDir) {
					t.Errorf("profile %s must be refused, got %v", profile, err)
				}
				if err != nil && strings.Contains(err.Error(), "mask") {
					t.Errorf("profile %s blames a mask: %v", profile, err)
				}
			}

			_, code, err := sb.Run("printf LOS60A-EMPTY", 10, ProfileStandard)
			if err == nil {
				t.Fatalf("the standard run started (exit=%d), an empty work dir would inherit the daemon's cwd", code)
			}
			if !runNeverStarted(err) {
				t.Errorf("run error %q is not classified as a run that never started", err)
			}
			if text := maskRefusalForCaller(err); text == "" || strings.ContainsAny(text, "\r\n") {
				t.Errorf("caller text is empty or carries a raw line break: %q", text)
			}
		})
	}
}

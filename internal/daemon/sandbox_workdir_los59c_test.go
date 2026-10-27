package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-428 (OPS-104): ai-jail binds the run's cwd read-write AFTER the caller's
// mounts. A work dir that is the home or an ancestor of it therefore shadows every
// read-only map inside it, and those maps are the whole reason a cap run gets them:
// the yesmem CLI the store()/llm() polyfills call and the restricted job socket.
// Los 58h closed the same shadowing for ~/.claude.json and mapped instead of
// masking; the CLI and the socket cannot be masked (the run needs them), so the
// run is refused instead.
func TestLos59cCoveringWorkDirIsRefused(t *testing.T) {
	home := isolateHome(t)
	ancestor := filepath.Dir(home)
	inside := filepath.Join(home, "yesmem-run", "workdir", "cap:probe")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatalf("create %s: %v", inside, err)
	}
	link := filepath.Join(t.TempDir(), "ancestor-link")
	if err := os.Symlink(ancestor, link); err != nil {
		t.Skipf("symlink not supported here: %v", err)
	}

	cases := []struct {
		name string
		work string
	}{
		{"the home itself", home},
		{"an ancestor of the home", ancestor},
		{"an ancestor reached through a symlink", link},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			los34cPinAiJail(t, 0)
			h, _ := mustHandler(t)
			sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, WorkDir: tc.work})
			if sb.cfg.PrepareErr == nil {
				t.Fatalf("a work dir that covers the home (%s) prepared without a refusal", tc.work)
			}
			if !errors.Is(sb.cfg.PrepareErr, errWorkDirCoversHome) {
				t.Errorf("PrepareErr %q does not carry errWorkDirCoversHome", sb.cfg.PrepareErr)
			}

			out, code, err := sb.Run("printf LOS59C-RAN", 10, ProfileStandard)
			if err == nil {
				t.Fatalf("the standard run started (exit=%d out=%q), the covering cwd bind would shadow the CLI and the job socket", code, out)
			}
			if !errors.Is(err, errWorkDirCoversHome) {
				t.Errorf("run error %q does not carry errWorkDirCoversHome", err)
			}
			// The run never started, so the auto-correct pass must not read this as
			// a script failure (bash_error_handler.go, handler_scheduler.go).
			if !runNeverStarted(err) {
				t.Errorf("run error %q is not classified as a run that never started", err)
			}
			if code != -1 || out != "" {
				t.Errorf("a refused run reported exit=%d out=%q, want -1 and no output", code, out)
			}

			// The refusal has to be actionable: the operator sees the work dir and
			// the reason without the daemon log. The home stays out of it — the
			// caller never supplied that path (Befund 5, YM-334).
			text := maskRefusalForCaller(err)
			if !strings.Contains(text, tc.work) {
				t.Errorf("caller text %q does not name the work dir %q", text, tc.work)
			}
			if !strings.Contains(text, "home") {
				t.Errorf("caller text %q does not name the reason", text)
			}
			if strings.Contains(text, "mask") {
				t.Errorf("caller text %q blames a mask, the operator would look in the mask list instead of the work dir", text)
			}
			if tc.work != home && strings.Contains(text, home) {
				t.Errorf("caller text %q names the home %q, a host path the caller never held", text, home)
			}
			if strings.Contains(text, "\n") {
				t.Errorf("caller text carries a line break, a path could inject a second line: %q", text)
			}
		})
	}
}

// The negative control: the refusal must be about "the home lies inside the work
// dir", not about a work dir that merely exists. A directory inside the home is the
// shape every cap run has (<dataDir>-run/workdir/cap:NAME), and a plain temp dir is
// the shape every job has. Refusing those would take the sandbox off every run.
func TestLos59cNonCoveringWorkDirStillRuns(t *testing.T) {
	home := isolateHome(t)
	inside := filepath.Join(home, "yesmem-run", "workdir", "cap:probe")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatalf("create %s: %v", inside, err)
	}

	cases := []struct {
		name string
		work string
	}{
		{"a directory inside the home (the cap shape)", inside},
		{"a directory beside the home (the job shape)", t.TempDir()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			los34cPinAiJail(t, 0)
			h, _ := mustHandler(t)
			sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, WorkDir: tc.work})
			if sb.cfg.PrepareErr != nil {
				t.Fatalf("a work dir that does not cover the home was refused: %v", sb.cfg.PrepareErr)
			}
			out, code, err := sb.Run("printf LOS59C-RAN", 10, ProfileStandard)
			if err != nil || code != 0 {
				t.Fatalf("run: exit=%d err=%v out=%q", code, err, out)
			}
			if strings.TrimSpace(out) != "LOS59C-RAN" {
				t.Errorf("run output = %q, want the probe", out)
			}
		})
	}
}

// The profile decides, not the presence of PrepareErr: a work dir at the home is a
// legitimate run under `sandbox: none` — that profile starts the command directly
// and binds nothing, so it has no mount to shadow and no cwd bind to leak through.
// newSandbox records the refusal for the profiles that bind; it must not refuse the
// none run.
func TestLos59cCoveringWorkDirStaysAllowedWithoutASandbox(t *testing.T) {
	home := isolateHome(t)
	h, _ := mustHandler(t)
	sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, WorkDir: home})
	if sb.cfg.PrepareErr == nil {
		t.Fatal("the work-dir refusal is not recorded, so the none profile is not the only one observed here")
	}

	out, code, err := sb.Run("printf LOS59C-NONE", 10, ProfileNone)
	if err != nil || code != 0 {
		t.Fatalf("a none run with the home as work dir must stay allowed: exit=%d err=%v out=%q", code, err, out)
	}
	if strings.TrimSpace(out) != "LOS59C-NONE" {
		t.Errorf("run output = %q, want the probe", out)
	}
}

// A strict run renders no --mask at all (profileRendersMasks), and the refusal still
// has to reach it: strict binds the run's cwd as well, and with the home as work dir
// the real home stays readable inside the run (measured on 0.9.2, YM-428). The mask
// branch of maskFailure keeps its own, narrower boundary (requiredMountErr documents
// it); only none is exempt from the work-dir refusal.
func TestLos59cCoveringWorkDirRefusalCoversEveryBindingProfile(t *testing.T) {
	home := isolateHome(t)
	sb := NewSandbox(SandboxConfig{Enabled: true, PrepareErr: workDirCoversHomeErr(home)})

	if err := sb.maskFailure(ProfileNone); err != nil {
		t.Errorf("the none profile binds nothing, it must not be refused: %v", err)
	}
	for _, profile := range []SandboxProfile{ProfileStandard, ProfileStrict} {
		err := sb.maskFailure(profile)
		if !errors.Is(err, errWorkDirCoversHome) {
			t.Errorf("profile %s must be refused, got %v", profile, err)
		}
		if err != nil && strings.Contains(err.Error(), "mask") {
			t.Errorf("profile %s blames a mask: %v", profile, err)
		}
	}
	// The mask cause keeps its old boundary: strict renders no mask, so a mask
	// failure alone does not refuse it.
	maskOnly := NewSandbox(SandboxConfig{Enabled: true, PrepareErr: fmt.Errorf("placeholder /nowhere: denied")})
	if err := maskOnly.maskFailure(ProfileStrict); err != nil {
		t.Errorf("a mask cause must not refuse a strict run: %v", err)
	}
	if err := maskOnly.maskFailure(ProfileStandard); !errors.Is(err, errMaskPrepare) {
		t.Errorf("a mask cause must refuse the standard profile, got %v", err)
	}
}

// A path with a line break or a control character must not reach a job result or a
// cap response as a second line: the message is built with %q, so it cannot.
func TestLos59cWorkDirRefusalTextIsOneLine(t *testing.T) {
	hostile := "/home/alice/pro\njects\x1b[2K"
	text := maskRefusalForCaller(workDirCoversHomeErr(hostile))
	if text == "" {
		t.Fatal("the caller text is empty")
	}
	if strings.ContainsAny(text, "\n\r\x1b") {
		t.Errorf("caller text carries a raw line break or escape: %q", text)
	}
	if !strings.Contains(text, `pro\njects`) {
		t.Errorf("the work dir did not reach the text quoted, so a path could break the line: %q", text)
	}
	if !strings.Contains(text, "home") {
		t.Errorf("caller text %q does not name the reason", text)
	}
	// The other prepare refusals keep their path-free text (Befund 5, YM-334).
	if plain := maskRefusalForCaller(fmt.Errorf("%w: mask placeholder /home/alice/.claude.json: denied", errMaskPrepare)); strings.Contains(plain, "/home/alice") {
		t.Errorf("a mask refusal names a host path again: %q", plain)
	}
}

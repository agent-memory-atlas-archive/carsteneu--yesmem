package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-15 / D2b: standard and strict must differ and both must enforce something
// under the ai-jail the host actually has (0.9.2). standard stays out of
// lockdown so that a cap can still reach the daemon socket; what it can enforce
// without lockdown is Landlock, seccomp and the masked secret paths.
func TestSandboxFlags_StandardMasksSecretsWithoutLockdown(t *testing.T) {
	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{
		MaskedPaths:    []string{"/home/u/.ssh", "/home/u/.gnupg"},
		ReadOnlyMounts: []string{"/home/u/.local/bin/yesmem", "/home/u/.claude/yesmem/daemon.sock"},
	}}

	_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStandard)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	joined := strings.Join(args, " ")

	// Lockdown would cut caps off from the daemon socket (ai-jail ignores --map
	// inside lockdown), so standard must not use it.
	if strings.Contains(joined, "--lockdown") {
		t.Errorf("standard must not lock down, got %v", args)
	}
	if !strings.Contains(joined, "--map /home/u/.claude/yesmem/daemon.sock") {
		t.Errorf("standard must mount the daemon socket read-only, got %v", args)
	}
	for _, want := range []string{"--landlock", "--seccomp", "--mask /home/u/.ssh", "--mask /home/u/.gnupg"} {
		if !strings.Contains(joined, want) {
			t.Errorf("standard must pass %s, got %v", want, args)
		}
	}
	if strings.Contains(joined, "--rw-map") {
		t.Errorf("no writable mounts allowed, got %v", args)
	}
}

// strict is the lockdown profile: read-only everywhere except /tmp. It adds no
// masks and no port grants. Note (ai-jail 0.9.2): lockdown keeps 22/80/443 open
// on its own, so strict is not a "no network" promise.
func TestSandboxFlags_StrictLocksDown(t *testing.T) {
	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{
		MaskedPaths: []string{"/home/u/.ssh"},
	}}

	_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileStrict)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--lockdown") {
		t.Errorf("strict must pass --lockdown, got %v", args)
	}
	if strings.Contains(joined, "--mask") || strings.Contains(joined, "--allow-tcp-port") {
		t.Errorf("strict adds nothing beyond lockdown, got %v", args)
	}
}

// ai-jail must not persist its effective flags into the .ai-jail of its working
// directory: caps run with the user's home as cwd, jobs inside repositories, and
// a persisted file hijacks later flag-less ai-jail sessions started there.
func TestSandboxFlags_NeverSavesConfig(t *testing.T) {
	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail"}

	for _, profile := range []SandboxProfile{ProfileStandard, ProfileStrict} {
		_, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, profile)
		if err != nil {
			t.Fatalf("wrap %s: %v", profile, err)
		}
		if !strings.Contains(strings.Join(args, " "), "--no-save-config") {
			t.Errorf("profile %s must pass --no-save-config, got %v", profile, args)
		}
	}
}

func TestWrapExecArgs_NoneIsPassThrough(t *testing.T) {
	s := &Sandbox{binaryPath: "/usr/local/bin/ai-jail", cfg: SandboxConfig{AllowedPorts: []int{80, 443}}}

	bin, args, err := s.WrapExecArgs("bash", []string{"-c", "true"}, ProfileNone)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	if bin != "bash" || len(args) != 2 {
		t.Errorf("ProfileNone must be pass-through, got bin=%q args=%v", bin, args)
	}
}

// Every mask the sandbox renders has to exist: ai-jail treats a missing --mask
// path as a silent no-op (measured on 0.9.2), so defaultSecretMasks materialises
// what is missing instead of dropping it (YM-291). HOME is pinned because that
// call now creates the paths it lists.
func TestDefaultSecretMasksOnlyReturnsExistingAbsolutePaths(t *testing.T) {
	isolateHomeAt(t, t.TempDir())
	for _, mask := range defaultSecretMasks() {
		if _, err := os.Stat(mask); err != nil {
			t.Errorf("mask %q does not exist, ai-jail would ignore it", mask)
		}
		if !filepath.IsAbs(mask) {
			t.Errorf("mask %q must be absolute", mask)
		}
	}
}

// YM-165: a non-zero exit must surface as an error. The scheduler records a run
// as "ok" whenever err == nil, which is how a bash job with a syntax error was
// booked as successful 25 times while auto-correction never fired.
func TestRunWithProfile_NonZeroExitReturnsError(t *testing.T) {
	s := NewSandbox(SandboxConfig{Enabled: false})

	out, code, err := s.RunWithProfile("echo boom >&2; exit 42", 10, ProfileNone)

	if code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
	if err == nil {
		t.Error("expected non-nil error for exit 42, got nil (run would be recorded as ok)")
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("output = %q, want captured stderr", out)
	}
}

// YM-16: a requested profile without ai-jail must fail closed when the sandbox
// is configured to refuse the unsandboxed fallback.
func TestRunWithProfile_FailClosedWithoutAiJail(t *testing.T) {
	_, cleanup := withLookupSpy(t, true)
	defer cleanup()

	s := NewSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false})

	out, code, err := s.RunWithProfile("echo should-not-run", 10, ProfileStandard)

	if err == nil {
		t.Fatal("expected error: profile standard without ai-jail and fallback disabled")
	}
	if strings.Contains(out, "should-not-run") {
		t.Errorf("command ran unsandboxed: %q", out)
	}
	if code != -1 {
		t.Errorf("exit code = %d, want -1", code)
	}
}

// FallbackUnsandboxed=true stays a documented opt-out, not a silent one.
func TestRunWithProfile_FallbackAllowedStillRuns(t *testing.T) {
	_, cleanup := withLookupSpy(t, true)
	defer cleanup()

	s := NewSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: true})

	out, code, err := s.RunWithProfile("echo fallback-ran", 10, ProfileStandard)

	if err != nil {
		t.Fatalf("fallback allowed, but got error: %v", err)
	}
	if code != 0 || !strings.Contains(out, "fallback-ran") {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

// The fallback path needs the same exit-code handling as the sandboxed one.
func TestRunWithProfile_FallbackPathReportsNonZeroExit(t *testing.T) {
	_, cleanup := withLookupSpy(t, true)
	defer cleanup()

	s := NewSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: true})

	_, code, err := s.RunWithProfile("exit 7", 10, ProfileStandard)

	if code != 7 {
		t.Errorf("exit = %d, want 7", code)
	}
	if err == nil {
		t.Error("expected error for exit 7 on the fallback path")
	}
}

// t.Chdir keeps the .ai-jail file ai-jail writes into the working directory out
// of the repository.
func realAiJailSandbox(t *testing.T, cfg SandboxConfig) *Sandbox {
	t.Helper()
	requireBubblewrap(t)
	t.Chdir(t.TempDir())
	s := NewSandbox(cfg)
	if !s.Available() {
		t.Skip("ai-jail not available, cannot exercise a real sandboxed run")
	}
	return s
}

// The generated flags have to be accepted by the real ai-jail — an unknown flag
// makes the whole run fail instead of sandboxing it.
func TestRunWithProfile_StandardLaunchesUnderRealAiJail(t *testing.T) {
	// defaultSecretMasks materialises missing placeholders (YM-291); a fake home
	// keeps this test off the developer's own.
	isolateHomeAt(t, t.TempDir())
	s := realAiJailSandbox(t, SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		MaskedPaths:         defaultSecretMasks(),
		ReadOnlyMounts:      []string{"/usr/bin/curl"},
	})

	out, code, err := s.RunWithProfile("echo sandboxed-ok", 30, ProfileStandard)
	if err != nil {
		t.Fatalf("standard profile failed under real ai-jail: %v (out=%q)", err, out)
	}
	if code != 0 || !strings.Contains(out, "sandboxed-ok") {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

func TestRunWithProfile_StrictLaunchesUnderRealAiJail(t *testing.T) {
	s := realAiJailSandbox(t, SandboxConfig{Enabled: true, FallbackUnsandboxed: false})

	out, code, err := s.RunWithProfile("echo strict-ok", 30, ProfileStrict)
	if err != nil {
		t.Fatalf("strict profile failed under real ai-jail: %v (out=%q)", err, out)
	}
	if code != 0 || !strings.Contains(out, "strict-ok") {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

// The mask is the part of standard that is actually observable: the yesmem
// install state holds an API key and is readable from the sandbox unless it is
// masked. Runs both ways so the test cannot pass for the wrong reason.
func TestRunWithProfile_StandardMaskHidesInstallState(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	state := filepath.Join(home, ".claude", "yesmem", "install-state.json")
	if _, err := os.Stat(state); err != nil {
		t.Skip("no install-state.json on this host")
	}

	probe := `printf 'INSTALLSTATE_SIZE=%s\n' "$(stat -c %s ` + state + ` 2>/dev/null || echo missing)"`

	unmasked := realAiJailSandbox(t, SandboxConfig{Enabled: true, FallbackUnsandboxed: false})
	out, _, err := unmasked.RunWithProfile(probe, 30, ProfileStandard)
	if err != nil {
		t.Fatalf("unmasked run: %v", err)
	}
	if got := probeValue(t, out, "INSTALLSTATE_SIZE"); got == "0" || got == "missing" {
		t.Skipf("install-state.json is not readable in the sandbox anyway (%q), mask not observable", got)
	}

	masked := realAiJailSandbox(t, SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		MaskedPaths:         []string{state},
	})
	out, code, err := masked.RunWithProfile(probe, 30, ProfileStandard)
	if err != nil {
		t.Fatalf("masked run: %v", err)
	}
	if code != 0 {
		t.Fatalf("probe exit = %d (out=%q)", code, out)
	}
	if got := probeValue(t, out, "INSTALLSTATE_SIZE"); got != "0" {
		t.Errorf("masked install-state.json is still readable (size %q), want 0", got)
	}
}

// probeValue pulls a KEY=value line out of the captured output. ai-jail writes
// its status banner to stderr, and CombinedOutput merges that into the result.
func probeValue(t *testing.T, out, key string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("probe %s not found in output %q", key, out)
	return ""
}

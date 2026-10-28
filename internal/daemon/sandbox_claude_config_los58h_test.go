package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// YM-291 (Befund 1): ~/.claude.json carries mcpServers, and Claude Code starts
// those commands at its next launch. The sandbox maps the file read-only when the
// run has a work dir; TestJobSandboxHidesTheClaudeGlobalConfigByRunCwd only proves
// the flag is rendered. That is not the same claim: ai-jail lays its own home binds
// after the caller's mounts, so a map can be shadowed and the write would still land
// on the host. This measures the effect instead of the argv.
func TestLos58hRealRunCannotWriteTheClaudeGlobalConfig(t *testing.T) {
	home := isolateHome(t)
	globalConfig := claudeGlobalConfig(home)
	body := `{"mcpServers":{"evil":{"command":"id"}}}` + "\n"
	if err := os.WriteFile(globalConfig, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", globalConfig, err)
	}
	// A work dir outside the home: with one the run maps the file read-only, and
	// only a run that inherits the home as its cwd would shadow every mount.
	work := t.TempDir()
	h, _ := mustHandler(t)
	sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, WorkDir: work})
	if !sb.Available() {
		t.Skip("ai-jail not available, cannot exercise a real sandboxed run")
	}
	quoted := los34dShellQuote(globalConfig)

	// Readable, and with its real size: the map keeps the file usable for a
	// sandboxed `claude -p`, while a mask would hand it 0 bytes and abort the CLI.
	out, code, err := sb.RunWithProfile(fmt.Sprintf("wc -c < %s", quoted), 30, ProfileStandard)
	if err != nil || code != 0 {
		t.Fatalf("read probe failed: exit=%d err=%v out=%q", code, err, out)
	}
	if got := strings.TrimSpace(out); got != strconv.Itoa(len(body)) {
		t.Errorf("config size inside the run = %q, want %d (a 0-byte file means the map became a mask)", got, len(body))
	}

	// The write that would add an mcpServers command must not reach the host.
	sb.RunWithProfile(fmt.Sprintf("printf EVIL >> %s", quoted), 30, ProfileStandard)
	if got := los34dReadFile(t, globalConfig); got != body {
		t.Fatalf("the run wrote into ~/.claude.json, the read-only map did not hold: got %q", got)
	}

	// Control: an unmapped file in the run's own work dir accepts the same write,
	// so the refusal above is the map and not an unreachable path. The home itself
	// is a private tmpfs in this run, so a control there would prove nothing.
	control := filepath.Join(work, "los58h-control.txt")
	if err := os.WriteFile(control, []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write control: %v", err)
	}
	controlOut, controlCode, controlErr := sb.RunWithProfile(fmt.Sprintf("printf EVIL >> %s", los34dShellQuote(control)), 30, ProfileStandard)
	if controlErr != nil || controlCode != 0 {
		t.Fatalf("control write failed: exit=%d err=%v out=%q", controlCode, controlErr, controlOut)
	}
	if !strings.Contains(los34dReadFile(t, control), "EVIL") {
		t.Fatal("the control write did not land on the host, so the probe cannot tell a refused write from an unreachable path")
	}
}

// YM-291 (Befund 1), second half: ai-jail binds the run's cwd read-write AFTER the
// caller's mounts, so a work dir that covers the config shadows its map and the
// write reaches the host (measured: with the home as work dir the append landed on
// ~/.claude.json). The empty-work-dir branch already masks for that reason; a work
// dir that covers the file is the same case and has to take the same branch, or the
// ticket's own finding comes back silently.
//
// Since YM-428 this branch is no longer reached through the handler: a work dir that
// covers ~/.claude.json covers the home, and newSandbox refuses such a run before it
// starts (TestLos58hRealRunWithTheHomeAsCwdIsRefused). It stays because NewSandbox
// is also built directly (tests, and the shape a caller would use), and because the
// mask is still the right answer for a Sandbox that carries no PrepareErr.
func TestLos58hClaudeConfigIsMaskedWhenTheCwdBindWouldShadowIt(t *testing.T) {
	home := isolateHome(t)
	globalConfig := claudeGlobalConfig(home)
	if err := os.WriteFile(globalConfig, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", globalConfig, err)
	}
	los34cPinAiJail(t, 0)
	sb := NewSandbox(SandboxConfig{Enabled: true, ClaudeConfig: globalConfig, WorkDir: home})

	flags := strings.Join(sb.sandboxFlags(ProfileStandard), " ")
	if strings.Contains(flags, "--map "+globalConfig) {
		t.Errorf("the config is mapped although the cwd bind covers it; flags = %v", flags)
	}
	if !strings.Contains(flags, "--mask "+globalConfig) {
		t.Errorf("the config is neither mapped nor masked; flags = %v", flags)
	}
}

// The same case, second half: YM-428 (OPS-104) took the mask branch away from this
// shape. A work dir that is the home makes ai-jail bind the whole home read-write
// over the sandbox, which shadows not only this file's map but also the maps of the
// yesmem CLI and the job socket — and those two cannot be masked, the run needs
// them. So the run is refused before it starts instead of run with a mask. The
// branch above (claudeConfigFlags) still renders the mask for a caller that builds
// a Sandbox directly; through the handler the refusal comes first.
func TestLos58hRealRunWithTheHomeAsCwdIsRefused(t *testing.T) {
	home := isolateHome(t)
	globalConfig := claudeGlobalConfig(home)
	body := `{"mcpServers":{"evil":{"command":"id"}}}` + "\n"
	if err := os.WriteFile(globalConfig, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", globalConfig, err)
	}
	// A stand-in ai-jail, so the result does not depend on a binary the test host
	// may lack: Run checks availability before the refusal, and without a binary it
	// would answer "unsandboxed" and hide what this test measures.
	los34cPinAiJail(t, 0)
	h, _ := mustHandler(t)
	sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, WorkDir: home})

	out, code, err := sb.RunWithProfile(fmt.Sprintf("printf EVIL >> %s", los34dShellQuote(globalConfig)), 30, ProfileStandard)
	if err == nil {
		t.Fatalf("the run started with the home as work dir (exit=%d out=%q), its cwd bind would shadow the CLI and the job-socket maps", code, out)
	}
	if !errors.Is(err, errWorkDirCoversHome) {
		t.Fatalf("run refused with %q, want errWorkDirCoversHome", err)
	}
	if code != -1 || out != "" {
		t.Errorf("a refused run reported exit=%d out=%q, want -1 and no output", code, out)
	}
}

// The other half of the evidence chain, and the reason the refusal exists: without
// it the same run really does bind the home read-write. Built directly, so the
// Sandbox carries no refusal (that is what the handler adds), and measured against
// the installed ai-jail: the home's control file accepts the append while the config
// takes the mask branch and does not. Skipped when no ai-jail is available; the flags
// alone are covered by the two tests above.
func TestLos58hRealRunBindsTheHomeAndMasksTheConfigWithoutTheRefusal(t *testing.T) {
	home := isolateHome(t)
	globalConfig := claudeGlobalConfig(home)
	body := `{"mcpServers":{"evil":{"command":"id"}}}` + "\n"
	if err := os.WriteFile(globalConfig, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", globalConfig, err)
	}
	control := filepath.Join(home, "los58h-control.txt")
	if err := os.WriteFile(control, []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write control: %v", err)
	}
	sb := NewSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, ClaudeConfig: globalConfig, WorkDir: home})
	if !sb.Available() {
		t.Skip("ai-jail not available, cannot exercise a real sandboxed run")
	}

	out, code, err := sb.RunWithProfile(fmt.Sprintf("printf EVIL >> %s; printf EVIL >> %s", los34dShellQuote(globalConfig), los34dShellQuote(control)), 30, ProfileStandard)
	t.Logf("write attempt: exit=%d err=%v out=%q", code, err, out)
	if got := los34dReadFile(t, globalConfig); got != body {
		t.Fatalf("the config changed: got %q", got)
	}
	if !strings.Contains(los34dReadFile(t, control), "EVIL") {
		t.Fatal("the control write did not land, so the home is not bound here and the probe proves nothing about the shadowing")
	}
}

// And the shape that is reached through a symlink: ai-jail binds the cwd the kernel
// reports, which is resolved, while the configured work dir is the link. Without
// resolving, the comparison misses it and the map is shadowed again.
func TestLos58hClaudeConfigIsMaskedWhenTheWorkDirReachesItViaSymlink(t *testing.T) {
	home := isolateHome(t)
	globalConfig := claudeGlobalConfig(home)
	if err := os.WriteFile(globalConfig, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", globalConfig, err)
	}
	link := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlink not supported here: %v", err)
	}
	los34cPinAiJail(t, 0)
	sb := NewSandbox(SandboxConfig{Enabled: true, ClaudeConfig: globalConfig, WorkDir: link})

	flags := strings.Join(sb.sandboxFlags(ProfileStandard), " ")
	if strings.Contains(flags, "--map "+globalConfig) {
		t.Errorf("the config is mapped although the resolved work dir covers it; flags = %v", flags)
	}
}

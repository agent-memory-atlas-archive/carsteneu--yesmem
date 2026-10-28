package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// los34dClaudeStatePaths is the spec list of Claude Code state a sandboxed run
// must not see (YM-284): the caps directory (the daemon re-imports a rewritten
// CAP.md and runs it unsandboxed elsewhere), the two settings files and the hook
// scripts they call (executed at every Claude Code start), and the two provider
// key files. It is spelled out here instead of being read off the implementation,
// so a shrinking default cannot pass silently.
func los34dClaudeStatePaths(home string) []string {
	claude := filepath.Join(home, ".claude")
	return []string{
		filepath.Join(claude, "caps"),
		filepath.Join(claude, "settings.json"),
		filepath.Join(claude, "settings.local.json"),
		filepath.Join(claude, "hooks"),
		filepath.Join(claude, "config.json"),
		filepath.Join(home, ".local", "share", "opencode", "auth.json"),
	}
}

// los34dWriteDecoyTree lays down every path of the spec list below home, with
// short recognisable content per file.
func los34dWriteDecoyTree(t *testing.T, home string) {
	t.Helper()
	claude := filepath.Join(home, ".claude")
	for _, dir := range []string{
		filepath.Join(claude, "caps"),
		filepath.Join(claude, "hooks"),
		filepath.Join(home, ".local", "share", "opencode"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	files := map[string]string{
		filepath.Join(claude, "caps", "CAP.md"):                         "cap-body\n",
		filepath.Join(claude, "hooks", "notify.sh"):                     "hook-body\n",
		filepath.Join(claude, "settings.json"):                          "settings-body\n",
		filepath.Join(claude, "settings.local.json"):                    "local-body\n",
		filepath.Join(claude, "config.json"):                            "config-body\n",
		filepath.Join(home, ".local", "share", "opencode", "auth.json"): "auth-body\n",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// YM-284: every path of the spec list has to be an ai-jail mask on the sandbox a
// cap or job actually gets, and the mask has to appear in the rendered flags of
// both major versions — 0.9.x is the host that needs it today (its standard
// profile does not lock the home down), 2.x renders the same list.
func TestJobSandboxMasksClaudeControlPaths(t *testing.T) {
	for _, major := range []int{0, 2} {
		t.Run(fmt.Sprintf("ai-jail-%dx", major), func(t *testing.T) {
			home := t.TempDir()
			los34dWriteDecoyTree(t, home)
			los34cPinAiJail(t, major)
			h, _ := mustHandler(t)
			// os.UserHomeDir reads HOME, so the default list resolves against the
			// decoy tree. Set after mustHandler, which does not depend on it.
			isolateHomeAt(t, home)

			sb := h.newSandbox(SandboxConfig{Enabled: true})

			masked := make(map[string]bool, len(sb.cfg.MaskedPaths))
			for _, path := range sb.cfg.MaskedPaths {
				masked[path] = true
			}
			flags := los34dStandardFlags(t, sb)
			// A pinned major that did not resolve would render the other flag set
			// and the mask check below would pass for the wrong reason.
			if got, want := strings.Contains(flags, "--no-network"), major == 2; got != want {
				t.Fatalf("flag set does not match the pinned ai-jail major %d, flags = %v", major, flags)
			}
			for _, want := range los34dClaudeStatePaths(home) {
				if !masked[want] {
					t.Errorf("%s is not masked for a job sandbox, masked = %v", want, sb.cfg.MaskedPaths)
				}
				if !strings.Contains(flags, "--mask "+want) {
					t.Errorf("standard profile does not render the mask for %s, flags = %v", want, flags)
				}
			}
		})
	}
}

// YM-284: the same check against this host's real home. Skips when the host has
// none of the paths (a fresh install without caps or provider keys).
func TestDefaultSecretMasksCoverThisHostsClaudeState(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	// Count what this host carries before asking for the masks: the call
	// materialises missing paths (YM-291, defaultSecretMasks), so afterwards every
	// path of the spec list exists and the skip below would be unreachable.
	present := 0
	for _, path := range los34dClaudeStatePaths(home) {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		present++
	}
	if present == 0 {
		t.Skip("this host carries none of the Claude Code state paths")
	}

	// The mask lists are read directly rather than through defaultSecretMasks():
	// that call materialises missing paths (YM-291) and would write into this host's
	// home from a test.
	masked := maskPathsForHome(home)

	for _, path := range los34dClaudeStatePaths(home) {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if !masked[path] {
			t.Errorf("%s exists on this host but is not in the mask list", path)
		}
	}
}

// YM-284 against the real binary. The decoys live below ~/.claude: ai-jail 0.9.x
// does not mount the whole home, it binds selected dotdirs, so only a path there
// is reachable from the sandbox at all. The unmasked run proves that
// reachability, otherwise the masked run could pass for the wrong reason. Both
// write attempts are expected to land on the host only in the unmasked run.
func TestRunWithProfile_ClaudeStateMaskHidesControlPathsForReal(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	root, err := os.MkdirTemp(filepath.Join(home, ".claude"), "los34d-masks-")
	if err != nil {
		t.Skipf("no writable ~/.claude: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	decoyHome := filepath.Join(root, "home")
	los34dWriteDecoyTree(t, decoyHome)
	capsDir := filepath.Join(decoyHome, ".claude", "caps")
	hooksDir := filepath.Join(decoyHome, ".claude", "hooks")
	settingsFile := filepath.Join(decoyHome, ".claude", "settings.json")
	configFile := filepath.Join(decoyHome, ".claude", "config.json")
	authFile := filepath.Join(decoyHome, ".local", "share", "opencode", "auth.json")
	hostFiles := []string{
		filepath.Join(capsDir, "CAP.md"),
		filepath.Join(hooksDir, "notify.sh"),
		settingsFile,
		configFile,
		authFile,
	}
	masks := []string{}
	for _, path := range los34dClaudeStatePaths(decoyHome) {
		if _, err := os.Stat(path); err == nil {
			masks = append(masks, path)
		}
	}
	if len(masks) != len(los34dClaudeStatePaths(decoyHome)) {
		t.Fatalf("the decoy tree is incomplete, masks = %v", masks)
	}

	probe := strings.Join([]string{
		`printf 'CAPS_COUNT=%s\n' "$(ls -A ` + los34dShellQuote(capsDir) + ` 2>/dev/null | wc -l)"`,
		`echo injected >> ` + los34dShellQuote(filepath.Join(capsDir, "CAP.md")) + ` 2>/dev/null || true`,
		`printf 'HOOKS_COUNT=%s\n' "$(ls -A ` + los34dShellQuote(hooksDir) + ` 2>/dev/null | wc -l)"`,
		`echo injected >> ` + los34dShellQuote(filepath.Join(hooksDir, "notify.sh")) + ` 2>/dev/null || true`,
		`printf 'SETTINGS_SIZE=%s\n' "$(stat -c %s ` + los34dShellQuote(settingsFile) + ` 2>/dev/null || echo missing)"`,
		`echo injected >> ` + los34dShellQuote(settingsFile) + ` 2>/dev/null || true`,
		`printf 'CONFIG_SIZE=%s\n' "$(stat -c %s ` + los34dShellQuote(configFile) + ` 2>/dev/null || echo missing)"`,
		`echo injected >> ` + los34dShellQuote(configFile) + ` 2>/dev/null || true`,
		`printf 'AUTH_SIZE=%s\n' "$(stat -c %s ` + los34dShellQuote(authFile) + ` 2>/dev/null || echo missing)"`,
		`echo injected >> ` + los34dShellQuote(authFile) + ` 2>/dev/null || true`,
	}, "; ")

	unmasked := realAiJailSandbox(t, SandboxConfig{Enabled: true, FallbackUnsandboxed: false})
	out, code, err := unmasked.RunWithProfile(probe, 30, ProfileStandard)
	if err != nil {
		t.Fatalf("unmasked run: %v (out=%q)", err, out)
	}
	if code != 0 {
		t.Fatalf("unmasked probe exit = %d (out=%q)", code, out)
	}
	if got := probeValue(t, out, "CAPS_COUNT"); got != "1" {
		t.Skipf("the decoy tree is not reachable from the sandbox (caps=%q), mask not observable", got)
	}
	for _, file := range hostFiles {
		if !los34dContainsInjected(t, file) {
			t.Fatalf("unmasked run did not reach %s, the masked run would prove nothing", file)
		}
		// Reset the decoys, otherwise every host file already carries the marker.
		body := strings.ReplaceAll(los34dReadFile(t, file), "injected\n", "")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatalf("reset %s: %v", file, err)
		}
	}

	masked := realAiJailSandbox(t, SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		MaskedPaths:         masks,
	})
	out, code, err = masked.RunWithProfile(probe, 30, ProfileStandard)
	if err != nil {
		t.Fatalf("masked run: %v (out=%q)", err, out)
	}
	if code != 0 {
		t.Fatalf("masked probe exit = %d (out=%q)", code, out)
	}
	for _, key := range []string{"CAPS_COUNT", "HOOKS_COUNT"} {
		if got := probeValue(t, out, key); got != "0" {
			t.Errorf("%s = %q inside the sandbox, want 0 (the directory still shows entries)", key, got)
		}
	}
	for _, key := range []string{"SETTINGS_SIZE", "CONFIG_SIZE", "AUTH_SIZE"} {
		got := probeValue(t, out, key)
		if got != "0" && got != "missing" {
			t.Errorf("%s = %q inside the sandbox, want 0 or missing (the file is still readable)", key, got)
		}
	}
	for _, file := range hostFiles {
		if los34dContainsInjected(t, file) {
			t.Errorf("%s was written from inside the sandbox, the mask did not hold", file)
		}
	}
}

// YM-284: a caller that supplies its own mask list must not drop the defaults.
// The daemon appends them, which is what keeps a single caller-local mask from
// silently reopening the provider-key and Claude-state paths.
func TestJobSandboxKeepsTheDefaultMasksWhenTheCallerBringsOne(t *testing.T) {
	home := t.TempDir()
	los34dWriteDecoyTree(t, home)
	los34cPinAiJail(t, 2)
	h, _ := mustHandler(t)
	isolateHomeAt(t, home)

	callerMask := filepath.Join(t.TempDir(), "caller-only")
	if err := os.WriteFile(callerMask, []byte("x"), 0o600); err != nil {
		t.Fatalf("write caller mask: %v", err)
	}
	sb := h.newSandbox(SandboxConfig{Enabled: true, MaskedPaths: []string{callerMask}})

	masked := make(map[string]bool, len(sb.cfg.MaskedPaths))
	for _, path := range sb.cfg.MaskedPaths {
		masked[path] = true
	}
	if !masked[callerMask] {
		t.Errorf("the caller's own mask %q was dropped, masked = %v", callerMask, sb.cfg.MaskedPaths)
	}
	for _, want := range los34dClaudeStatePaths(home) {
		if !masked[want] {
			t.Errorf("%s was dropped when the caller supplied a mask, masked = %v", want, sb.cfg.MaskedPaths)
		}
	}
}

// appendMissingPaths must return a fresh slice: the caller's list keeps its
// length, and nothing lands in its spare capacity. It also drops duplicates the
// caller already carries.
func TestAppendMissingPathsKeepsTheCallerSliceIntact(t *testing.T) {
	caller := make([]string, 2, 4)
	caller[0], caller[1] = "a", "a"

	out := appendMissingPaths(caller, "b", "a", "")

	if got, want := strings.Join(out, ","), "a,b"; got != want {
		t.Fatalf("appendMissingPaths = %q, want %q", got, want)
	}
	if len(caller) != 2 {
		t.Fatalf("caller slice length = %d, want 2", len(caller))
	}
	if spare := caller[:cap(caller)][2]; spare != "" {
		t.Errorf("wrote %q into the caller's spare capacity, the returned slice is not fresh", spare)
	}
}

// YM-284: caps_dir may point somewhere other than the default beside the data
// dir. The daemon imports CAP.md from the configured directory, so that is the
// one the sandbox must not reach.
func TestJobSandboxMasksTheConfiguredCapsDir(t *testing.T) {
	los34cPinAiJail(t, 0)
	h, _ := mustHandler(t)
	configured := filepath.Join(t.TempDir(), "configured-caps")
	if err := os.MkdirAll(configured, 0o755); err != nil {
		t.Fatalf("mkdir configured caps dir: %v", err)
	}
	h.capsDir = configured

	isolateHomeAt(t, t.TempDir())
	sb := h.newSandbox(SandboxConfig{Enabled: true})

	found := false
	for _, path := range sb.cfg.MaskedPaths {
		if path == configured {
			found = true
		}
	}
	if !found {
		t.Fatalf("the configured caps dir (%q) is not masked, masked = %v", configured, sb.cfg.MaskedPaths)
	}
}

// YM-284: the CAP.md re-import channel is the directory save_cap writes to, which
// follows the data dir. Masking only the home's default caps dir would leave a
// moved data dir exposed.
func TestJobSandboxMasksTheCapsDirBesideTheDataDir(t *testing.T) {
	los34cPinAiJail(t, 0)
	h, _ := mustHandler(t)
	capsDir := filepath.Join(filepath.Dir(h.dataDir), "caps")
	if err := os.MkdirAll(capsDir, 0o755); err != nil {
		t.Fatalf("mkdir caps dir: %v", err)
	}

	isolateHomeAt(t, t.TempDir())
	sb := h.newSandbox(SandboxConfig{Enabled: true})

	found := false
	for _, path := range sb.cfg.MaskedPaths {
		if path == capsDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("the caps dir beside the data dir (%q) is not masked, masked = %v", capsDir, sb.cfg.MaskedPaths)
	}
	if flags := los34dStandardFlags(t, sb); !strings.Contains(flags, "--mask "+capsDir) {
		t.Errorf("standard profile does not render the caps dir mask, flags = %v", flags)
	}
}

// YM-284: masking ~/.claude/settings.json removes the proxy URL a sandboxed
// claude run used to read from there. The daemon's own environment carries it and
// has to hand it to the run, spelled out for ai-jail 2.x (no env inheritance).
func TestJobSandboxForwardsTheProxyURL(t *testing.T) {
	los34cPinAiJail(t, 2)
	h, _ := mustHandler(t)
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9099")

	isolateHomeAt(t, t.TempDir())
	sb := h.newSandbox(SandboxConfig{Enabled: true})

	found := false
	for _, kv := range sb.cfg.Env {
		if kv == "ANTHROPIC_BASE_URL=http://127.0.0.1:9099" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the proxy URL is not forwarded into the run, env = %v", sb.cfg.Env)
	}
	if flags := los34dStandardFlags(t, sb); !strings.Contains(flags, "--env ANTHROPIC_BASE_URL") {
		t.Errorf("ai-jail 2.x does not get the env forward, flags = %v", flags)
	}
}

// los34dStandardFlags renders the standard flag set after resolving the binary,
// the way production does (handleExecuteCap calls Available before rendering).
// Without it the version probe never runs, aiJailMajor stays at its zero value
// and the legacy set is rendered even for a pinned 2.x binary.
func los34dStandardFlags(t *testing.T, sb *Sandbox) string {
	t.Helper()
	if !sb.Available() {
		t.Fatal("the stand-in ai-jail is not resolvable")
	}
	return strings.Join(sb.sandboxFlags(ProfileStandard), " ")
}

// los34dShellQuote wraps a path for the probe command: a home with a space or a
// quote would otherwise break the command instead of testing the mask.
func los34dShellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

func los34dReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func los34dContainsInjected(t *testing.T, path string) bool {
	t.Helper()
	return strings.Contains(los34dReadFile(t, path), "injected")
}

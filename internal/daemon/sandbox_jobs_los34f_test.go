package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// los34fPinAiJailScript resolves a stand-in ai-jail that runs script instead of
// the command. A script that prints its own argv makes the rendered flag set
// observable without a real ai-jail.
func los34fPinAiJailScript(t *testing.T, major int, script string) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "ai-jail")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write stand-in ai-jail: %v", err)
	}
	origLookup, origProbe := sandboxLookPath, sandboxVersionProbe
	sandboxLookPath = func(string) (string, error) { return binary, nil }
	sandboxVersionProbe = func(string) int { return major }
	t.Cleanup(func() {
		sandboxLookPath, sandboxVersionProbe = origLookup, origProbe
	})
}

// los34fPinExecAiJail drops ai-jail's flags and execs the command they precede,
// like the los34b/los34c stand-ins.
func los34fPinExecAiJail(t *testing.T, major int) {
	t.Helper()
	los34fPinAiJailScript(t, major, "#!/bin/sh\nwhile [ $# -gt 0 ] && [ \"$1\" != \"bash\" ]; do shift; done\nshift\nexec bash \"$@\"\n")
}

// los34fPinEchoAiJail prints every argument it receives and exits. The cap's
// output is then the flag set the daemon rendered.
func los34fPinEchoAiJail(t *testing.T, major int) {
	t.Helper()
	los34fPinAiJailScript(t, major, "#!/bin/sh\nfor a in \"$@\"; do printf 'ARG:%s\\n' \"$a\"; done\n")
}

// los34fWriteOpencodeConfig lays down an opencode config with one explicit model
// and one provider that only carries its default.
func los34fWriteOpencodeConfig(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	body := `{"provider":{"zai":{"models":{"glm-5.2":{}}},"other":{"models":{}}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// los34fEnvValue returns the last value for key in an env list built by cfg.Env.
func los34fEnvValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		name, rest, ok := strings.Cut(kv, "=")
		if !ok || name != key {
			continue
		}
		value, found = rest, true
	}
	return value, found
}

// los34fMasked reports whether path is masked on the built sandbox.
func los34fMasked(sb *Sandbox, path string) bool {
	for _, mask := range sb.cfg.MaskedPaths {
		if mask == path {
			return true
		}
	}
	return false
}

// los34fCoveredByMask reports whether path, or a directory above it, is masked.
func los34fCoveredByMask(sb *Sandbox, path string) bool {
	for _, mask := range sb.cfg.MaskedPaths {
		if mask == path || pathInside(mask, path) {
			return true
		}
	}
	return false
}

// los34fOpencodeModelEntries plants a home whose opencode config names two
// providers, and returns that home.
func los34fOpencodeModelEntries(t *testing.T) string {
	t.Helper()
	home := isolateHome(t)
	los34fWriteOpencodeConfig(t, home)
	return home
}

// YM-303 (0): since YM-291 both ai-jail majors start a run in its work dir, and a
// job's work dir is the project. A project .ai-jail is then read and merged over
// the flags the daemon renders: the yesmem checkout carries a tracked one whose
// command is a telegram poll, and its mask/rw_maps/hide_dotdirs keys would widen
// the sandbox. --clean makes ai-jail ignore that file (measured on 0.9.2: the
// repo mask applied without it and did not apply with it).
func TestLos34fLegacyFlagsIgnoreTheProjectAiJail(t *testing.T) {
	for _, profile := range []SandboxProfile{ProfileStandard, ProfileStrict} {
		t.Run(fmt.Sprintf("profile-%v", profile), func(t *testing.T) {
			los34cPinAiJail(t, 0)
			isolateHome(t)
			h, _ := mustHandler(t)

			sb := h.newSandbox(SandboxConfig{Enabled: true})
			flags := sb.sandboxFlags(profile)
			if !los34fContainsFlag(flags, "--clean") {
				t.Fatalf("0.9.x %s flags = %v, want --clean (a project .ai-jail is read otherwise)", profile, flags)
			}
		})
	}
}

// los34fContainsFlag reports whether the flag list carries want.
func los34fContainsFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
}

// YM-303 (1): ~/.config/opencode/opencode.json is the opencode write channel —
// plugin paths and mcpServers in it become code at the next opencode start. It
// stayed readable because two bundled caps read it for their /models command. The
// file is masked now and the model list reaches a run through its environment, so
// the command keeps working without handing back the file.
func TestLos34fMasksTheOpencodeConfigAndServesTheModelList(t *testing.T) {
	los34cPinAiJail(t, 0)
	home := los34fOpencodeModelEntries(t)
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	configDir := filepath.Join(home, ".config", "opencode")
	configPath := filepath.Join(configDir, "opencode.json")
	if !los34fMasked(sb, configDir) {
		t.Fatalf("the opencode config directory is not masked, masks = %v", sb.cfg.MaskedPaths)
	}
	// The directory mask is what covers the sibling names opencode also reads:
	// opencode.jsonc takes priority over opencode.json and config.json is the third
	// candidate, so a per-file mask left two writable names in place.
	for _, sibling := range []string{"opencode.jsonc", "config.json"} {
		if !los34fCoveredByMask(sb, filepath.Join(configDir, sibling)) {
			t.Errorf("%s is not covered by a mask, masks = %v", sibling, sb.cfg.MaskedPaths)
		}
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("the config has to survive as a placeholder, stat: %v", err)
	}

	models, ok := los34fEnvValue(sb.cfg.Env, "YESMEM_OPENCODE_MODELS")
	if !ok {
		t.Fatalf("the run carries no YESMEM_OPENCODE_MODELS, env = %v", sb.cfg.Env)
	}
	// The list replaces what the caps used to read out of the file.
	for _, want := range []string{"zai/glm-5.2", "other/* (default)"} {
		if !strings.Contains(models, want) {
			t.Errorf("model list %q does not carry %q", models, want)
		}
	}
}

// YM-303 (2): ~/.claude/backups holds copies of ~/.claude.json, which carries
// mcpServers. Claude Code writes into that directory when it aborts, so single
// files cannot cover it; the directory is masked as a whole.
func TestLos34fMasksTheClaudeBackupsDir(t *testing.T) {
	los34cPinAiJail(t, 0)
	home := isolateHome(t)
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	backups := filepath.Join(home, ".claude", "backups")
	if !los34fMasked(sb, backups) {
		t.Fatalf("~/.claude/backups is not masked, masks = %v", sb.cfg.MaskedPaths)
	}
	info, err := os.Stat(backups)
	if err != nil {
		t.Fatalf("the placeholder has to exist, stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("the placeholder for ~/.claude/backups has to be a directory")
	}
}

// YM-303 (4): the key files are masked, their backups were not — and a backup
// carries the same key. Every sibling of a masked credential file is masked too.
func TestLos34fMasksCredentialBackupSiblings(t *testing.T) {
	los34cPinAiJail(t, 0)
	home := isolateHome(t)
	openCodeData := filepath.Join(home, ".local", "share", "opencode")
	files := []string{
		filepath.Join(home, ".claude", "settings.json.bak-20261017-sessionend"),
		filepath.Join(openCodeData, "auth.json.bak-20260906-222810"),
		filepath.Join(openCodeData, "account.json.backup"),
	}
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(file), err)
		}
		if err := os.WriteFile(file, []byte(`{"key":"sk-live"}`), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	for _, file := range files {
		if !los34fMasked(sb, file) {
			t.Errorf("%s carries a key but is not masked, masks = %v", file, sb.cfg.MaskedPaths)
		}
	}
	// A path that does not exist is not invented: the list is existence filtered,
	// so no empty .bak appears in the user's home.
	if _, err := os.Stat(filepath.Join(openCodeData, "auth.json.bak-20200101")); !os.IsNotExist(err) {
		t.Errorf("a nonexistent backup was materialised, stat err = %v", err)
	}
}

// YM-303 (3): ~/.claude/config.json carried a live provider key with mode 0777 on
// this host. The daemon reads that file at start anyway, so it is the place that
// takes the key out of reach of other local users.
func TestLos34fHardensTheClaudeConfigFileMode(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".claude", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"primaryApiKey":"sk-live"}`), 0o777); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatalf("chmod config: %v", err)
	}

	if got := ReadClaudeCodeAPIKey(); got != "sk-live" {
		t.Fatalf("ReadClaudeCodeAPIKey = %q, want the key", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o after the read, want 600", perm)
	}
}

// YM-303 (5): Claude Code reads .mcp.json and .claude/settings.local.json from the
// working directory upward, and ai-jail binds the cwd read-write. A home mask does
// not reach them. They are masked at the run's own work dir, and only when they
// exist: a placeholder would plant a file in the user's repository.
func TestLos34fMasksProjectLocalClaudeConfigs(t *testing.T) {
	los34cPinAiJail(t, 0)
	isolateHome(t)
	h, _ := mustHandler(t)

	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, file := range []string{
		filepath.Join(work, ".mcp.json"),
		filepath.Join(work, ".claude", "settings.local.json"),
	} {
		if err := os.WriteFile(file, []byte(`{"permissions":{"allow":[]}}`), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}

	sb := h.newSandbox(SandboxConfig{Enabled: true, WorkDir: work})

	for _, file := range []string{
		filepath.Join(work, ".mcp.json"),
		filepath.Join(work, ".claude", "settings.local.json"),
	} {
		if !los34fMasked(sb, file) {
			t.Errorf("%s is readable from the run, masks = %v", file, sb.cfg.MaskedPaths)
		}
	}
	missing := filepath.Join(work, ".claude", "settings.json")
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("a placeholder was written into the project directory: %s (err=%v)", missing, err)
	}
}

// YM-303 (6): XDG_CONFIG_HOME and XDG_DATA_HOME redirect every agent tool. Both
// were inherited from the daemon's environment, so a value there moved the config
// out from under the home masks entirely.
func TestLos34fPinsTheXdgDirectories(t *testing.T) {
	los34cPinAiJail(t, 0)
	home := isolateHome(t)
	unsetEnvForTest(t, "XDG_CONFIG_HOME")
	unsetEnvForTest(t, "XDG_DATA_HOME")
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	for key, want := range map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
	} {
		got, ok := los34fEnvValue(sb.cfg.Env, key)
		if !ok {
			t.Errorf("%s is not pinned, env = %v", key, sb.cfg.Env)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// YM-303 (6), the other half: pinning alone would move the tool somewhere else. A
// host that keeps its config under a custom XDG root has to get that root masked —
// the file there is the same write channel as the one in the home default.
func TestLos34fMasksTheEffectiveXdgRoots(t *testing.T) {
	los34cPinAiJail(t, 0)
	isolateHome(t)
	root := t.TempDir()
	configRoot := filepath.Join(root, "xdg-config")
	dataRoot := filepath.Join(root, "xdg-data")
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)
	configPath := filepath.Join(configRoot, "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{"provider":{}}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	authPath := filepath.Join(dataRoot, "opencode", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(authPath, []byte(`{"key":"sk-live"}`), 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	for _, path := range []string{configPath, authPath, filepath.Join(configRoot, "opencode", "plugin")} {
		if !los34fCoveredByMask(sb, path) {
			t.Errorf("%s sits under the effective XDG root and is not masked, masks = %v", path, sb.cfg.MaskedPaths)
		}
	}
	if got, _ := los34fEnvValue(sb.cfg.Env, "XDG_CONFIG_HOME"); got != configRoot {
		t.Errorf("XDG_CONFIG_HOME pinned to %q, want the host value %q", got, configRoot)
	}
}

// unsetEnvForTest removes a variable for the duration of the test.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(key, orig)
		}
	})
}

// YM-303 (7): a caller-provided mask list went into the flags unexamined. A mask
// path that does not exist is a silent no-op in ai-jail, so the entry protected
// nothing until the path happened to exist — and a run could create it.
func TestLos34fMaterialisesCallerMaskedPaths(t *testing.T) {
	los34cPinAiJail(t, 0)
	isolateHome(t)
	h, _ := mustHandler(t)

	callerMask := filepath.Join(t.TempDir(), "caller-mask.json")
	sb := h.newSandbox(SandboxConfig{Enabled: true, MaskedPaths: []string{callerMask}})

	if !los34fMasked(sb, callerMask) {
		t.Fatalf("the caller mask was dropped, masks = %v", sb.cfg.MaskedPaths)
	}
	if _, err := os.Stat(callerMask); err != nil {
		t.Fatalf("the caller mask was not materialised, stat: %v", err)
	}
}

// YM-303 (8): a placeholder that cannot be created used to be a warning and the
// run went ahead — with a mask ai-jail ignores, so exactly the channel the list
// exists to close stayed open. The run is refused instead.
func TestLos34fFailsClosedWhenAPlaceholderCannotBeCreated(t *testing.T) {
	los34fPinExecAiJail(t, 0)
	isolateHome(t)
	h, _ := mustHandler(t)

	// A regular file where a directory has to be: creating the parent of the
	// placeholder fails with ENOTDIR.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	impossible := filepath.Join(blocker, "child", "mask.json")

	sb := h.newSandbox(SandboxConfig{Enabled: true, MaskedPaths: []string{impossible}})

	out, code, err := sb.RunWithProfile("echo RAN", 10, ProfileStandard)
	if err == nil {
		t.Fatalf("the run was not refused, exit=%d out=%q", code, out)
	}
	if strings.Contains(out, "RAN") {
		t.Fatalf("the command ran despite the broken mask, out=%q", out)
	}
	if !strings.Contains(err.Error(), impossible) {
		t.Errorf("error %q does not name the mask path %q", err, impossible)
	}
}

// YM-303 (10a): the proxy URL reached a run only through the daemon's own
// environment. A daemon started without it left a sandboxed `claude -p` without a
// route, and the proxy config that says where the proxy listens was ignored. The
// URL is derived from that port when the environment carries none.
func TestLos34fDerivesTheProxyURLWhenTheEnvironmentHasNone(t *testing.T) {
	los34cPinAiJail(t, 0)
	isolateHome(t)
	h, _ := mustHandler(t)
	h.proxyForwardPort = 9191

	orig, had := os.LookupEnv("ANTHROPIC_BASE_URL")
	if err := os.Unsetenv("ANTHROPIC_BASE_URL"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv("ANTHROPIC_BASE_URL", orig)
		}
	})

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	got, ok := los34fEnvValue(sb.cfg.Env, "ANTHROPIC_BASE_URL")
	if !ok {
		t.Fatalf("no ANTHROPIC_BASE_URL in the run environment, env = %v", sb.cfg.Env)
	}
	if got != "http://127.0.0.1:9191" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want http://127.0.0.1:9191", got)
	}
}

// YM-303 (10b): a cap run forwards no loopback port. On ai-jail 2.x the sandbox
// has its own loopback, so a cap that reaches the proxy (a bash cap running
// `claude -p`) had no route — the same relay the bash-job and headless paths
// already declare.
func TestLos34fCapExecutionForwardsTheProxyPort(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	los34fPinEchoAiJail(t, 2)
	isolateHome(t)
	h, _ := mustHandler(t)
	h.proxyForwardPort = 9191
	insertCapWithSandbox(t, h, "los34f_flags", "true", "standard")

	out := los34fCapOutput(t, h, "los34f_flags")
	if !strings.Contains(out, "ARG:--forward-port") || !strings.Contains(out, "ARG:9191") {
		t.Fatalf("cap flags carry no forwarded proxy port, output:\n%s", out)
	}
}

// YM-303 (1): the model list has to arrive in the cap's own process. ai-jail 2.x
// only forwards the name of an env var and 0.9.x inherits the process environment,
// so the value has to be on the command either way.
func TestLos34fCapExecutionDeliversTheModelListToTheProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	los34fPinExecAiJail(t, 0)
	los34fOpencodeModelEntries(t)
	h, _ := mustHandler(t)
	insertCapWithSandbox(t, h, "los34f_env", `printf '%s' "${YESMEM_OPENCODE_MODELS:-MISSING}"`, "standard")

	out := los34fCapOutput(t, h, "los34f_env")
	if strings.TrimSpace(out) == "MISSING" || !strings.Contains(out, "zai/glm-5.2") {
		t.Fatalf("the cap process has no model list, output: %q", out)
	}
}

// los34fCapOutput runs one cap and returns its output.
func los34fCapOutput(t *testing.T, h *Handler, name string) string {
	t.Helper()
	resp := h.handleExecuteCap(map[string]any{"name": name, "fn": "run", "args": "{}"})
	if resp.Error != "" {
		t.Fatalf("execute %s: %s", name, resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	out, _ := result["output"].(string)
	return out
}

// YM-303 (10d): only the data dir was protected against a mount that re-exposes a
// masked path. With every agent-state path masked, the same invariant has to hold
// for all of them: a read-only map below a mask would put that path back.
func TestLos34fDropsMountsInsideAnyMaskedPath(t *testing.T) {
	los34cPinAiJail(t, 0)
	home := isolateHome(t)
	h, _ := mustHandler(t)

	maskedDir := filepath.Join(home, ".claude", "caps")
	if err := os.MkdirAll(maskedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inside := filepath.Join(maskedDir, "yesmem")
	outside := filepath.Join(t.TempDir(), "yesmem")

	sb := h.newSandbox(SandboxConfig{
		Enabled:        true,
		ReadOnlyMounts: []string{inside, outside},
	})

	var kept []string
	for _, mount := range sb.cfg.ReadOnlyMounts {
		kept = append(kept, mount)
	}
	for _, mount := range kept {
		if mount == inside {
			t.Fatalf("a mount below the masked ~/.claude/caps survived, mounts = %v", kept)
		}
	}
	found := false
	for _, mount := range kept {
		if mount == outside {
			found = true
		}
	}
	if !found {
		t.Fatalf("the mount outside every mask was dropped, mounts = %v", kept)
	}
}

// YM-303 (5) against the real binary: the mask has to hold below the run's own
// working directory, which ai-jail binds read-write.
func TestLos34fRealRunMasksTheProjectLocalConfig(t *testing.T) {
	home := isolateHome(t)
	work := filepath.Join(home, "project")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mcpPath := filepath.Join(work, ".mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"evil":{"command":"id"}}}`), 0o600); err != nil {
		t.Fatalf("write .mcp.json: %v", err)
	}

	sb := realAiJailSandbox(t, SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		WorkDir:             work,
		MaskedPaths:         []string{mcpPath},
	})

	out, code, err := sb.RunWithProfile(`printf 'SIZE=%s\n' "$(wc -c < .mcp.json)"`, 30, ProfileStandard)
	if err != nil {
		t.Fatalf("run: %v (out=%q)", err, out)
	}
	if code != 0 {
		t.Fatalf("probe exit = %d (out=%q)", code, out)
	}
	// An unreachable path would report a missing file rather than 0 bytes; both
	// mean the content is gone, but only 0 proves the mask was applied.
	got := probeValue(t, out, "SIZE")
	if got != "0" {
		t.Fatalf("SIZE = %q inside the sandbox, want 0 (the project config is readable)", got)
	}
	out, _, err = sb.RunWithProfile(fmt.Sprintf("echo EVIL >> %s", los34dShellQuote(mcpPath)), 30, ProfileStandard)
	if err == nil {
		t.Logf("write attempt returned no error (out=%q)", out)
	}
	body, readErr := os.ReadFile(mcpPath)
	if readErr != nil {
		t.Fatalf("read .mcp.json: %v", readErr)
	}
	if strings.Contains(string(body), "EVIL") {
		t.Fatal("the run wrote into the project's .mcp.json, the mask did not hold")
	}
}

// YM-303 review (§5.4): masking single config files was a bypass. opencode
// resolves its global config as opencode.jsonc → opencode.json → config.json
// (internal/opencode/paths.go), so a run that found only opencode.json masked
// created opencode.jsonc on the host — and opencode reads that name first, which
// made the mask a no-op for the channel it was added for.
func TestLos34fRealRunCannotWriteTheOpencodeConfigSiblings(t *testing.T) {
	home := isolateHome(t)
	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(`{"provider":{}}`), 0o600); err != nil {
		t.Fatalf("write opencode.json: %v", err)
	}
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	h, _ := mustHandler(t)
	sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, WorkDir: work})
	if !sb.Available() {
		t.Skip("ai-jail not available, cannot exercise a real sandboxed run")
	}

	// Control: the directory is reachable from the run, so a failed write below is
	// the mask and not an invisible path.
	control := filepath.Join(configDir, "probe.txt")
	if out, code, err := sb.RunWithProfile(fmt.Sprintf("printf x > %s", los34dShellQuote(control)), 30, ProfileStandard); err != nil || code != 0 {
		t.Fatalf("control write failed (%v, exit=%d, out=%q) — the probe cannot tell a mask from an unreachable path", err, code, out)
	}

	for _, name := range []string{"opencode.jsonc", "config.json"} {
		path := filepath.Join(configDir, name)
		sb.RunWithProfile(fmt.Sprintf("printf '{}' > %s", los34dShellQuote(path)), 30, ProfileStandard)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("the run created %s in the masked config directory — opencode prefers that file", name)
		}
	}
}

// YM-303 review (§5.4): ~/.claude/config.json may be absent, and a run can create
// it. hardenSecretFileMode used to follow the path with os.Stat/os.Chmod, so a
// symlink or a hard link placed there aimed the chmod at an inode of the run's
// choosing — a directory loses its x bit and everything below it becomes
// unreachable.
func TestLos34fHardenDoesNotFollowLinks(t *testing.T) {
	home := isolateHome(t)
	configPath := filepath.Join(home, ".claude", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A directory as the target: 0755 → 0600 would strip the x bit.
	target := filepath.Join(home, "shared-dir")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(target, configPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	ReadClaudeCodeAPIKey()
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("target directory mode = %o, the chmod followed the symlink", perm)
	}

	if err := os.Remove(configPath); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	// A hard link to a file that has to stay readable.
	victim := filepath.Join(home, "victim.json")
	if err := os.WriteFile(victim, []byte(`{"primaryApiKey":"sk-other"}`), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Link(victim, configPath); err != nil {
		t.Fatalf("link: %v", err)
	}
	ReadClaudeCodeAPIKey()
	info, err = os.Stat(victim)
	if err != nil {
		t.Fatalf("stat victim: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("hard-linked file mode = %o, the chmod reached the other name", perm)
	}
}

// YM-303 review (§5.4): PrepareErr refuses a run to avoid a mask that would be a
// no-op. A strict profile renders no --mask at all — it hides the home with
// --lockdown — so refusing it costs the run for a channel it never had.
func TestLos34fMaskFailureOnlyRefusesProfilesWithMasks(t *testing.T) {
	los34fPinExecAiJail(t, 0)
	isolateHome(t)
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})
	sb.cfg.PrepareErr = fmt.Errorf("mask placeholder /nowhere: permission denied")

	if _, _, err := sb.RunWithProfile("echo RAN", 10, ProfileStrict); err != nil {
		t.Fatalf("a strict run has no mask and must not be refused: %v", err)
	}
	if _, _, err := sb.RunWithProfile("echo RAN", 10, ProfileStandard); err == nil {
		t.Fatal("a standard run carries the masks and has to be refused")
	}
}

// YM-303 review (§5.4): the backup enumeration reads the files a run can create.
// Unbounded it becomes ai-jail's argv, and one run could push it past the exec
// limit so that every later run fails before it starts.
func TestLos34fBoundsTheBackupMasks(t *testing.T) {
	los34cPinAiJail(t, 0)
	home := isolateHome(t)
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < maxBackupMasks+10; i++ {
		name := filepath.Join(claude, fmt.Sprintf("settings.json.bak-%03d", i))
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatalf("write backup: %v", err)
		}
	}
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	got := 0
	for _, mask := range sb.cfg.MaskedPaths {
		if strings.Contains(mask, ".bak-") {
			got++
		}
	}
	if got > maxBackupMasks {
		t.Fatalf("%d backup masks, want at most %d", got, maxBackupMasks)
	}
	if got == 0 {
		t.Fatal("no backup mask at all, the enumeration stopped working")
	}
}

// Nachreview (5.3/5.2): a mask refusal is an infrastructure error, not a script
// failure. It has to be distinguishable from a real run failure, or the
// auto-correct pass rewrites a live cap over a sandbox that never started.
func TestLos34fMaskRefusalIsAnInfrastructureError(t *testing.T) {
	los34fPinExecAiJail(t, 0)
	isolateHome(t)
	h, _ := mustHandler(t)

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	impossible := filepath.Join(blocker, "child", "mask.json")
	sb := h.newSandbox(SandboxConfig{Enabled: true, MaskedPaths: []string{impossible}})

	_, _, err := sb.RunWithProfile("true", 10, ProfileStandard)
	if err == nil {
		t.Fatal("the run was not refused")
	}
	if !errors.Is(err, errMaskPrepare) {
		t.Fatalf("error %q does not carry errMaskPrepare, the auto-correct pass would treat it as a script failure", err)
	}
	if errors.Is(err, errUnsandboxed) {
		t.Fatalf("error %q is classified as a missing sandbox", err)
	}
}

// Nachreview (5.3): the model list has to come from the same root the masks cover.
// Reading the home default while the host keeps its config under a custom
// XDG_CONFIG_HOME hands a run an empty list on exactly the hosts the XDG masks fix.
func TestLos34fModelListFollowsTheXdgRoot(t *testing.T) {
	los34cPinAiJail(t, 0)
	isolateHome(t)
	configRoot := filepath.Join(t.TempDir(), "xdg-config")
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	opencodeDir := filepath.Join(configRoot, "opencode")
	if err := os.MkdirAll(opencodeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"provider":{"zai":{"models":{"glm-5.2":{}}}}}`
	if err := os.WriteFile(filepath.Join(opencodeDir, "opencode.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	models, ok := los34fEnvValue(sb.cfg.Env, "YESMEM_OPENCODE_MODELS")
	if !ok || !strings.Contains(models, "zai/glm-5.2") {
		t.Fatalf("model list = %q (present=%v), want the entries from the XDG config", models, ok)
	}
}

// Nachreview (5.2 F5): the claude global config is mapped read-only rather than
// masked, because masking it aborts the CLI. Its backups carry the same mcpServers
// and nothing reads them, so they are masked — existence filtered, because a
// placeholder named ".bak" would look like a restore point.
func TestLos34fMasksTheClaudeGlobalConfigBackups(t *testing.T) {
	los34cPinAiJail(t, 0)
	home := isolateHome(t)
	if err := os.WriteFile(claudeGlobalConfig(home), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	backup := claudeGlobalConfig(home) + ".backup"
	if err := os.WriteFile(backup, []byte(`{"mcpServers":{"evil":{"command":"id"}}}`), 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	h, _ := mustHandler(t)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	if !los34fMasked(sb, backup) {
		t.Fatalf("%s carries the same mcpServers but is not masked, masks = %v", backup, sb.cfg.MaskedPaths)
	}
	if _, err := os.Stat(claudeGlobalConfig(home) + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a nonexistent backup was materialised: %v", err)
	}
}

// Nachreview (5.3): the caller already sets YESMEM_SOCK and newSandbox appends it
// again. Two entries become two --env flags on 2.x and two environment entries on
// 0.9.x; which one wins is a consumer detail, so the list carries each key once.
func TestLos34fEnvHasNoDuplicateKeys(t *testing.T) {
	los34cPinAiJail(t, 0)
	isolateHome(t)
	h, _ := mustHandler(t)

	sock := JobSocketPath(h.dataDir)
	sb := h.newSandbox(SandboxConfig{Enabled: true, Env: []string{"YESMEM_SOCK=" + sock}})

	count := 0
	for _, kv := range sb.cfg.Env {
		if strings.HasPrefix(kv, "YESMEM_SOCK=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("YESMEM_SOCK appears %d times in %v, want 1", count, sb.cfg.Env)
	}
}

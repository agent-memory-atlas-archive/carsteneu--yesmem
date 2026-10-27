package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// los34eAgentStatePaths is the spec list of host state a sandboxed job must not
// reach (YM-291), the entries Los 34d left open. Every one of them steers or
// executes code outside this sandbox:
//   - ~/.claude.json carries mcpServers, whose commands Claude Code starts on its
//     next launch.
//   - ~/.claude/commands, skills, agents, rules, output-styles and the two
//     instruction files are read as instructions or run as code by the next
//     session.
//   - ~/.config/opencode holds the opencode config plus the plugin directories
//     opencode discovers automatically.
//
// The list is spelled out here instead of being read off the implementation, so
// a shrinking default cannot pass silently.
func los34eAgentStatePaths(home string) []string {
	return append(los34eAgentStateDirs(home), los34eAgentStateFiles(home)...)
}

// los34eAgentStateDirs are the entries that are directories on a real host.
func los34eAgentStateDirs(home string) []string {
	claude := filepath.Join(home, ".claude")
	opencodeConfig := filepath.Join(home, ".config", "opencode")
	return []string{
		filepath.Join(claude, "commands"),
		filepath.Join(claude, "skills"),
		filepath.Join(claude, "agents"),
		filepath.Join(claude, "rules"),
		filepath.Join(claude, "output-styles"),
		filepath.Join(claude, "plugins"),
		filepath.Join(opencodeConfig, "plugin"),
		filepath.Join(opencodeConfig, "plugins"),
		filepath.Join(opencodeConfig, "agent"),
		filepath.Join(opencodeConfig, "commands"),
		filepath.Join(opencodeConfig, "prompts"),
		filepath.Join(opencodeConfig, "themes"),
	}
}

// los34eAgentStateFiles are the entries that are regular files on a real host.
// ~/.claude.json is not among them on purpose: it is mapped read-only instead of
// masked, because masking it aborts the Claude CLI (see
// TestJobSandboxMapsTheClaudeGlobalConfigReadOnly).
func los34eAgentStateFiles(home string) []string {
	claude := filepath.Join(home, ".claude")
	return []string{
		filepath.Join(claude, "CLAUDE.md"),
		filepath.Join(claude, "AGENTS.md"),
	}
}

// los34eOpencodeCredentialFiles are masked and materialised: nothing probes them
// for existence, and a job could otherwise plant an auth.json that the next
// opencode start reads as its own config.
func los34eOpencodeCredentialFiles(home string) []string {
	opencode := filepath.Join(home, ".local", "share", "opencode")
	return []string{
		filepath.Join(opencode, "auth.json"),
		filepath.Join(opencode, "account.json"),
		filepath.Join(opencode, "mcp-auth.json"),
	}
}

// los34eConditionalCredentialFiles hold a provider key and are masked only when
// they exist: setup's agent detection reads ~/.claude/config.json as a signal that
// Claude Code is installed.
func los34eConditionalCredentialFiles(home string) []string {
	return []string{filepath.Join(home, ".claude", "config.json")}
}

// maskPathsForHome is the set of paths a non-agent sandbox masks for this home,
// composed from the list functions. Tests use it instead of defaultSecretMasks(),
// which materialises what is missing and would therefore write into the real home.
func maskPathsForHome(home string) map[string]bool {
	paths := make(map[string]bool)
	for _, entry := range agentStateMasks(home) {
		paths[entry.path] = true
	}
	for _, entry := range opencodeCredentialMasks(home) {
		paths[entry.path] = true
	}
	for _, path := range agentCredentialMasks(home) {
		paths[path] = true
	}
	return paths
}

// los34eWriteAgentStateTree lays down every path of the spec lists below home.
func los34eWriteAgentStateTree(t *testing.T, home string) {
	t.Helper()
	for _, dir := range los34eAgentStateDirs(home) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		// A real directory carries something; an empty one would let a mask and a
		// missing directory look the same from inside.
		if err := os.WriteFile(filepath.Join(dir, "los34e-decoy"), []byte("decoy\n"), 0o600); err != nil {
			t.Fatalf("write decoy in %s: %v", dir, err)
		}
	}
	for _, file := range append(append(los34eAgentStateFiles(home), los34eOpencodeCredentialFiles(home)...), los34eConditionalCredentialFiles(home)...) {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(file), err)
		}
		if err := os.WriteFile(file, []byte("los34e-body\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	// The Claude global config is not masked any more, but the decoy still needs it:
	// newSandbox maps it read-only.
	if err := os.WriteFile(claudeGlobalConfig(home), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", claudeGlobalConfig(home), err)
	}
}

// YM-291: every path of the spec list has to be an ai-jail mask on the sandbox a
// cap or job actually gets, in both rendered flag sets. 0.9.x is the host that
// needs it today: its standard profile binds ~/.claude, ~/.config and
// ~/.local/share/opencode read-write and ~/.claude.json as a writable file bind
// (measured 17.10.2026).
func TestJobSandboxMasksAgentStatePaths(t *testing.T) {
	for _, major := range []int{0, 2} {
		t.Run(fmt.Sprintf("ai-jail-%dx", major), func(t *testing.T) {
			home := t.TempDir()
			los34eWriteAgentStateTree(t, home)
			los34cPinAiJail(t, major)
			h, _ := mustHandler(t)
			// os.UserHomeDir reads HOME, so the default list resolves against the
			// decoy tree.
			t.Setenv("HOME", home)

			sb := h.newSandbox(SandboxConfig{Enabled: true})

			masked := append([]string(nil), sb.cfg.MaskedPaths...)
			flags := los34dStandardFlags(t, sb)
			// A pinned major that did not resolve would render the other flag set
			// and the mask check below would pass for the wrong reason.
			if got, want := strings.Contains(flags, "--no-network"), major == 2; got != want {
				t.Fatalf("flag set does not match the pinned ai-jail major %d, flags = %v", major, flags)
			}
			for _, want := range append(append(los34eAgentStatePaths(home), los34eOpencodeCredentialFiles(home)...), los34eConditionalCredentialFiles(home)...) {
				if !los34eCoveredByMask(masked, want) {
					t.Errorf("%s is not masked for a job sandbox, masked = %v", want, sb.cfg.MaskedPaths)
				}
				if !los34eCoveredByMask(los34eRenderedMasks(flags), want) {
					t.Errorf("standard profile does not render the mask for %s, flags = %v", want, flags)
				}
			}
		})
	}
}

// YM-291 finding 5: a mask path that does not exist yet used to be dropped from
// the list, and ai-jail treats a missing --mask path as a silent no-op (measured
// on 0.9.2: the job created the file on the host and the run reported success).
// The default list therefore has to materialise a placeholder of the right type
// and keep the path, so a job can no longer create it.
func TestDefaultSecretMasksMaterialiseMissingAgentStatePaths(t *testing.T) {
	// A fresh temporary home holds none of the paths, which is the fresh-host case
	// the finding describes.
	home := t.TempDir()
	t.Setenv("HOME", home)

	masks := defaultSecretMasks()
	if len(masks) == 0 {
		t.Fatal("defaultSecretMasks returned nothing, the checks below would pass vacuously")
	}

	for _, want := range los34eAgentStatePaths(home) {
		if !los34eCoveredByMask(masks, want) {
			t.Errorf("%s was dropped because it did not exist yet, masked = %v", want, masks)
		}
	}
	// ai-jail masks a file and a directory alike, but the placeholder has to have
	// the type the path would have on a real host. A directory path materialised
	// as a file would make the daemon's own MkdirAll fail later. A path below a
	// masked directory gets no placeholder of its own: the directory mask covers it.
	for _, dir := range los34eAgentStateDirs(home) {
		if !los34eExactMask(masks, dir) {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("placeholder for %s is missing: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("placeholder for %s is not a directory, mode = %v", dir, info.Mode())
		}
	}
	for _, file := range append(los34eAgentStateFiles(home), los34eOpencodeCredentialFiles(home)...) {
		info, err := os.Stat(file)
		if err != nil {
			t.Errorf("placeholder for %s is missing: %v", file, err)
			continue
		}
		if info.IsDir() {
			t.Errorf("placeholder for %s is a directory, mode = %v", file, info.Mode())
		}
		if strings.HasSuffix(file, ".json") {
			// A zero-byte file is not valid JSON and outlives the run.
			if body := los34dReadFile(t, file); body != "{}\n" {
				t.Errorf("JSON placeholder %s carries %q, want an empty object", file, body)
			}
		}
	}
	// ~/.claude/config.json is the opposite case: never materialised, because
	// setup's agent detection probes it for existence.
	for _, file := range los34eConditionalCredentialFiles(home) {
		if _, err := os.Stat(file); err == nil {
			t.Errorf("%s was created on a host that never had it; setup's agent detection reads that as an install", file)
		}
	}
}

// YM-291: an existing path must never be rewritten. This runs on the construction
// path of every cap and job, and the paths are the user's own files (~/.claude.json
// carries the whole Claude Code state).
func TestCreateMaskPlaceholderNeverTouchesAnExistingPath(t *testing.T) {
	home := t.TempDir()
	los34eWriteAgentStateTree(t, home)

	claudeJSON := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte("keep-me\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", claudeJSON, err)
	}
	dirEntry := filepath.Join(home, ".claude", "commands")

	if err := createMaskPlaceholder(maskEntry{path: claudeJSON}); err != nil {
		t.Fatalf("createMaskPlaceholder on an existing file = %v, want nil", err)
	}
	if got := los34dReadFile(t, claudeJSON); got != "keep-me\n" {
		t.Errorf("the existing file was rewritten: got %q, want %q", got, "keep-me\n")
	}
	if err := createMaskPlaceholder(maskEntry{path: dirEntry, dir: true}); err != nil {
		t.Fatalf("createMaskPlaceholder on an existing dir = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dirEntry, "los34e-decoy")); err != nil {
		t.Errorf("the existing directory entry was removed: %v", err)
	}
}

// YM-291: ~/.claude.json carries mcpServers, whose commands Claude Code starts on
// its next launch. With a work dir the run starts outside the home, so the file can
// stay readable and only the write is blocked (a sandboxed `claude -p` keeps
// working). Without one the run inherits the daemon's cwd, ai-jail binds that
// read-write, and every mount below is shadowed — there the file has to be masked.
func TestJobSandboxHidesTheClaudeGlobalConfigByRunCwd(t *testing.T) {
	for _, major := range []int{0, 2} {
		t.Run(fmt.Sprintf("ai-jail-%dx-with-work-dir-maps", major), func(t *testing.T) {
			los34cPinAiJail(t, major)
			h, _ := mustHandler(t)
			home := t.TempDir()
			los34eWriteAgentStateTree(t, home)
			t.Setenv("HOME", home)

			sb := h.newSandbox(SandboxConfig{Enabled: true})
			globalConfig := claudeGlobalConfig(home)
			if sb.cfg.ClaudeConfig != globalConfig {
				t.Fatalf("ClaudeConfig = %q, want %q", sb.cfg.ClaudeConfig, globalConfig)
			}
			cwd := sb.runWorkDir()
			if cwd == "" {
				t.Fatalf("the sandbox has no work dir, so the mask branch would be used")
			}
			if los34cInside(home, cwd) {
				t.Fatalf("the work dir %q lies inside the home, the map would be shadowed", cwd)
			}
			flags := los34dStandardFlags(t, sb)
			if !strings.Contains(flags, "--map "+globalConfig) {
				t.Errorf("flags do not carry the read-only map, flags = %v", flags)
			}
			if strings.Contains(flags, "--mask "+globalConfig) {
				t.Errorf("the config is masked as well, which aborts a sandboxed claude, flags = %v", flags)
			}
		})
	}
	t.Run("no-work-dir-masks", func(t *testing.T) {
		los34cPinAiJail(t, 0)
		home := t.TempDir()
		globalConfig := claudeGlobalConfig(home)
		if err := os.WriteFile(globalConfig, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", globalConfig, err)
		}
		sb := NewSandbox(SandboxConfig{Enabled: true, ClaudeConfig: globalConfig})

		flags := los34dStandardFlags(t, sb)
		if !strings.Contains(flags, "--mask "+globalConfig) {
			t.Errorf("without a work dir the cwd bind shadows every mount, so the file must be masked; flags = %v", flags)
		}
	})
}

// YM-291: a missing ~/.claude.json is materialised before it is hidden, otherwise
// a job could create the file itself and the next start would read it.
func TestJobSandboxMaterialisesTheClaudeGlobalConfig(t *testing.T) {
	los34cPinAiJail(t, 0)
	h, _ := mustHandler(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	globalConfig := claudeGlobalConfig(home)
	if sb.cfg.ClaudeConfig != globalConfig {
		t.Fatalf("ClaudeConfig = %q, want %q (the placeholder was not picked up)", sb.cfg.ClaudeConfig, globalConfig)
	}
	body, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("the global config placeholder is missing: %v", err)
	}
	if string(body) != "{}\n" {
		t.Errorf("%s carries %q, want a valid empty config", globalConfig, string(body))
	}
}

// YM-291: a path whose placeholder cannot be created must stay in the mask list.
// Dropping it is what reopened the channel — ai-jail no-ops on a missing --mask
// path, so the next job could create the file for real.
func TestMaterialiseMaskPathsKeepsEntriesItCannotCreate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, placeholder creation cannot be made to fail")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	if err := os.Mkdir(claude, 0o500); err != nil {
		t.Fatalf("mkdir %s: %v", claude, err)
	}
	t.Cleanup(func() { os.Chmod(claude, 0o700) })

	masks := defaultSecretMasks()

	for _, want := range []string{
		filepath.Join(claude, "skills"),
		filepath.Join(claude, "settings.json"),
	} {
		found := false
		for _, path := range masks {
			if path == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was dropped, so the channel it closes stays open; masked = %v", want, masks)
		}
	}
	if _, err := os.Stat(filepath.Join(claude, "skills")); err == nil {
		t.Fatalf("%s/skills exists, the failure branch under test was not reached", claude)
	}
}

// YM-291: until this change the 0.9.x path returned no work dir and the run
// inherited the daemon's cwd, which is the home — ai-jail then binds that home
// read-write over the sandbox and every mount below it is shadowed. Both majors now
// start in the configured work dir.
func TestRunUsesTheWorkDirOnBothAiJailMajors(t *testing.T) {
	for _, major := range []int{0, 2} {
		t.Run(fmt.Sprintf("ai-jail-%dx", major), func(t *testing.T) {
			los34cPinAiJail(t, major)
			h, _ := mustHandler(t)
			t.Setenv("HOME", t.TempDir())
			workDir := t.TempDir()

			sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false, WorkDir: workDir})
			if !sb.Available() {
				t.Fatal("the stand-in ai-jail is not resolvable")
			}
			out, code, err := sb.RunWithProfile("pwd", 10, ProfileStandard)
			if err != nil || code != 0 {
				t.Fatalf("run: %v (code=%d out=%q)", err, code, out)
			}
			got := strings.TrimSpace(out)
			if resolved, err := filepath.EvalSymlinks(got); err == nil {
				got = resolved
			}
			want := workDir
			if resolved, err := filepath.EvalSymlinks(want); err == nil {
				want = resolved
			}
			if got != want {
				t.Errorf("run started in %q, want the work dir %q", got, want)
			}
		})
	}
}

// YM-291 against the real binary, both topologies:
//   - a cap or bash job inheriting the daemon's cwd gets the file masked, because
//     ai-jail's cwd bind shadows any mount below it (measured: with cwd = $HOME the
//     whole real home is bound read-write over the sandbox's tmpfs);
//   - a headless run, which starts in its job work dir, keeps it readable and the
//     write is refused — that is what lets a sandboxed `claude -p` start at all.
func TestRunWithProfile_ClaudeGlobalConfigHidesTheWriteForReal(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	root, err := os.MkdirTemp(filepath.Join(realHome, ".claude"), "los34e-ro-")
	if err != nil {
		t.Skipf("no writable ~/.claude: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	decoyHome := filepath.Join(root, "home")
	if err := os.MkdirAll(decoyHome, 0o755); err != nil {
		t.Fatalf("mkdir decoy home: %v", err)
	}
	globalConfig := claudeGlobalConfig(decoyHome)
	body := []byte("{\"marker\":\"los34e\"}\n")
	if err := os.WriteFile(globalConfig, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", globalConfig, err)
	}
	t.Setenv("HOME", decoyHome)

	quoted := los34dShellQuote(globalConfig)
	probe := strings.Join([]string{
		fmt.Sprintf(`printf 'SIZE=%%s\n' "$(stat -c %%s %s 2>/dev/null || echo missing)"`, quoted),
		fmt.Sprintf(`printf 'WRITE=%%s\n' "$(: >> %s 2>/dev/null && echo open || echo blocked)"`, quoted),
	}, "; ")

	t.Run("with-work-dir-maps", func(t *testing.T) {
		h, _ := mustHandler(t)
		sb := h.newSandbox(SandboxConfig{Enabled: true, FallbackUnsandboxed: false})
		if !sb.Available() {
			t.Skip("ai-jail not available")
		}
		out, code, err := sb.RunWithProfile(probe, 30, ProfileStandard)
		if err != nil || code != 0 {
			t.Fatalf("run: %v (code=%d out=%q)", err, code, out)
		}
		if got := probeValue(t, out, "SIZE"); got != fmt.Sprintf("%d", len(body)) {
			t.Errorf("%s reports size %q inside the sandbox, want %d (readable for claude)", globalConfig, got, len(body))
		}
		if got := probeValue(t, out, "WRITE"); got != "blocked" {
			t.Errorf("%s is writable inside the sandbox (%q), a job could plant mcpServers", globalConfig, got)
		}
	})

	t.Run("without-work-dir-masks", func(t *testing.T) {
		sb := realAiJailSandbox(t, SandboxConfig{
			Enabled:             true,
			FallbackUnsandboxed: false,
			ClaudeConfig:        globalConfig,
		})
		out, code, err := sb.RunWithProfile(probe, 30, ProfileStandard)
		if err != nil || code != 0 {
			t.Fatalf("run: %v (code=%d out=%q)", err, code, out)
		}
		// A mask presents the file as empty; that is the channel being closed.
		if got := probeValue(t, out, "SIZE"); got != "0" {
			t.Errorf("%s reports size %q inside the sandbox, want 0 (masked)", globalConfig, got)
		}
		if got := probeValue(t, out, "WRITE"); got != "blocked" {
			t.Errorf("%s is writable inside the sandbox (%q)", globalConfig, got)
		}
	})
}

// YM-291 against the real binary: the decoys have to be unreachable and
// unwritable once masked, and the unmasked control run proves they were
// reachable — otherwise the masked run could pass for the wrong reason. The decoy
// tree lives below ~/.claude because that is a read-write bind on 0.9.x; the home
// root itself is a private tmpfs there, so a decoy placed there would prove
// nothing.
func TestRunWithProfile_AgentStateMasksHoldForReal(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	root, err := os.MkdirTemp(filepath.Join(home, ".claude"), "los34e-masks-")
	if err != nil {
		t.Skipf("no writable ~/.claude: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	decoyHome := filepath.Join(root, "home")
	los34eWriteAgentStateTree(t, decoyHome)
	dirs := los34eAgentStateDirs(decoyHome)
	files := append(append(los34eAgentStateFiles(decoyHome), los34eOpencodeCredentialFiles(decoyHome)...), los34eConditionalCredentialFiles(decoyHome)...)
	masks := append(append(los34eAgentStatePaths(decoyHome), los34eOpencodeCredentialFiles(decoyHome)...), los34eConditionalCredentialFiles(decoyHome)...)

	// A directory is counted before the write attempt: a masked directory is an
	// empty tmpfs, so the write succeeds inside it and would make the count look
	// unchanged. The write is still attempted every time, and the host check after
	// the run is what proves it did not land. A file is measured after the write,
	// so the size reports whether the append went through.
	var probe []string
	for i, dir := range dirs {
		probe = append(probe,
			fmt.Sprintf(`printf 'DIR%d=%%s\n' "$(ls -A %s 2>/dev/null | wc -l)"`, i, los34dShellQuote(dir)),
			fmt.Sprintf(`touch %s/los34e-injected 2>/dev/null || true`, los34dShellQuote(dir)),
		)
	}
	for i, file := range files {
		probe = append(probe,
			fmt.Sprintf(`echo injected >> %s 2>/dev/null || true`, los34dShellQuote(file)),
			fmt.Sprintf(`printf 'FILE%d=%%s\n' "$(stat -c %%s %s 2>/dev/null || echo missing)"`, i, los34dShellQuote(file)),
		)
	}
	command := strings.Join(probe, "; ")

	unmasked := realAiJailSandbox(t, SandboxConfig{Enabled: true, FallbackUnsandboxed: false})
	out, code, err := unmasked.RunWithProfile(command, 30, ProfileStandard)
	if err != nil {
		t.Fatalf("unmasked run: %v (out=%q)", err, out)
	}
	if code != 0 {
		t.Fatalf("unmasked probe exit = %d (out=%q)", code, out)
	}
	if got := probeValue(t, out, "DIR0"); got != "1" {
		t.Skipf("the decoy tree is not reachable from the sandbox (entries=%q), mask not observable", got)
	}
	for _, file := range files {
		if !los34dContainsInjected(t, file) {
			t.Fatalf("unmasked run did not reach %s, the masked run would prove nothing", file)
		}
		// Reset the decoys, otherwise they already carry the marker.
		body := strings.ReplaceAll(los34dReadFile(t, file), "injected\n", "")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatalf("reset %s: %v", file, err)
		}
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "los34e-injected")); err != nil {
			t.Fatalf("unmasked run did not reach %s, the masked run would prove nothing", dir)
		}
		if err := os.Remove(filepath.Join(dir, "los34e-injected")); err != nil {
			t.Fatalf("reset %s: %v", dir, err)
		}
	}

	masked := realAiJailSandbox(t, SandboxConfig{
		Enabled:             true,
		FallbackUnsandboxed: false,
		MaskedPaths:         masks,
	})
	out, code, err = masked.RunWithProfile(command, 30, ProfileStandard)
	if err != nil {
		t.Fatalf("masked run: %v (out=%q)", err, out)
	}
	if code != 0 {
		t.Fatalf("masked probe exit = %d (out=%q)", code, out)
	}
	for i, dir := range dirs {
		if got := probeValue(t, out, fmt.Sprintf("DIR%d", i)); got != "0" {
			t.Errorf("%s still shows %q entries inside the sandbox, want 0", dir, got)
		}
	}
	for i, file := range files {
		// The decoys are non-empty, so a masked file reports exactly 0. Accepting
		// "missing" would hide the difference between "masked" and "never there".
		if got := probeValue(t, out, fmt.Sprintf("FILE%d", i)); got != "0" {
			t.Errorf("%s reports size %q inside the sandbox, want 0", file, got)
		}
	}
	for _, file := range files {
		if los34dContainsInjected(t, file) {
			t.Errorf("%s was written from inside the sandbox, the mask did not hold", file)
		}
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "los34e-injected")); err == nil {
			t.Errorf("%s received a file from inside the sandbox, the mask did not hold", dir)
		}
	}
}

// los34eCoveredByMask reports whether path itself or a directory above it is
// masked. A directory mask covers every name below it, which is what the opencode
// config directory relies on: opencode reads a name that is not in the list.
func los34eCoveredByMask(masks []string, path string) bool {
	for _, mask := range masks {
		if mask == path || pathInside(mask, path) {
			return true
		}
	}
	return false
}

// los34eExactMask reports whether path is in the list itself, not merely covered
// by a directory mask above it.
func los34eExactMask(masks []string, path string) bool {
	for _, mask := range masks {
		if mask == path {
			return true
		}
	}
	return false
}

// los34eRenderedMasks returns the paths a rendered, space-joined flag set masks.
func los34eRenderedMasks(flags string) []string {
	parts := strings.Split(flags, " ")
	var out []string
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "--mask" {
			out = append(out, parts[i+1])
		}
	}
	return out
}

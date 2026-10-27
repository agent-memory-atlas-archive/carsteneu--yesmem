package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los34gWriteExec writes an executable stand-in at path.
func los34gWriteExec(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// YM-314 (1): the four mount flags a cap adds at the call site went into the argv
// after newSandbox had run, so they skipped the invariant that drops a mount below
// a masked path: a binary that moved under ~/.claude/caps would have been mounted
// back in and re-exposed exactly what the mask hides. The mount list is registered
// on the Sandbox now, which applies the same filter as every other mount.
func TestLos34gCapMountsGoThroughTheMaskFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("needs bash on PATH")
	}
	los34fPinEchoAiJail(t, 0)
	home := isolateHome(t)
	h, _ := mustHandler(t)

	// bash below a masked directory: its mount has to be dropped. The directory is
	// masked by default (agentStateMasks masks ~/.claude/caps).
	maskedDir := filepath.Join(home, ".claude", "caps")
	maskedBin := filepath.Join(maskedDir, "bin")
	maskedBash := filepath.Join(maskedBin, "bash")
	los34gWriteExec(t, maskedBash)
	// curl outside every mask: the same code path must keep it. Without this
	// control a failed PATH lookup would look exactly like a dropped mount.
	outside := t.TempDir()
	curl := filepath.Join(outside, "curl")
	los34gWriteExec(t, curl)
	t.Setenv("PATH", maskedBin+string(os.PathListSeparator)+outside+string(os.PathListSeparator)+os.Getenv("PATH"))
	insertCapWithSandbox(t, h, "los34g_mount", "true", "standard")

	out := los34fCapOutput(t, h, "los34g_mount")

	if !strings.Contains(out, "ARG:"+curl) {
		t.Fatalf("the control mount %q is missing, a dropped mount cannot be told apart from a failed lookup, output:\n%s", curl, out)
	}
	sock := JobSocketPath(h.dataDir)
	if !strings.Contains(out, "ARG:"+sock) {
		t.Fatalf("the socket mount %q is missing from the flag set, output:\n%s", sock, out)
	}
	if strings.Contains(out, "ARG:"+maskedBash) {
		t.Fatalf("the cap mounts %q, which sits inside the masked %q: the mask is defeated", maskedBash, maskedDir)
	}
}

// YM-314 (2): Claude Code reads .mcp.json and .claude/settings*.json from its
// working directory upward, and ai-jail binds that directory read-write. A mask
// on the work dir alone left a config in the parent chain reachable wherever a
// parent directory is bound (the run that inherits the daemon's cwd, or a work dir
// inside a bound dotdir such as ~/.claude). With a plain work dir outside those
// trees the parent chain is not bound at all (measured 18.10.2026 on ai-jail
// 0.9.2: only the cwd is reachable, its parents are empty mount points), so the
// mask here is insurance and not a closed channel. Asserted on the list, because
// an end-to-end probe cannot pass where the path is unreachable.
func TestLos34gMasksProjectConfigsFromTheParentChain(t *testing.T) {
	los34cPinAiJail(t, 0)
	home := isolateHome(t)
	h, _ := mustHandler(t)

	project := filepath.Join(home, "projects", "app")
	work := filepath.Join(project, "sub")
	if err := os.MkdirAll(filepath.Join(project, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	parents := []string{
		filepath.Join(project, ".mcp.json"),
		filepath.Join(project, ".claude", "settings.local.json"),
	}
	for _, file := range parents {
		if err := os.WriteFile(file, []byte(`{"mcpServers":{"evil":{"command":"id"}}}`), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}

	sb := h.newSandbox(SandboxConfig{Enabled: true, WorkDir: work})

	for _, file := range parents {
		if !los34fMasked(sb, file) {
			t.Errorf("%s sits above the work dir and is readable from the run, masks = %v", file, sb.cfg.MaskedPaths)
		}
	}
	// Existing paths only: a placeholder in the user's project would be a file
	// this daemon planted there.
	missing := filepath.Join(project, ".claude", "settings.json")
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("a placeholder was written into the project: %s (err=%v)", missing, err)
	}
}

// YM-314 (2), the symlinked case: the run's cwd is the resolved directory, so a
// config above a resolved path has to be masked as well.
func TestLos34gMasksParentConfigsOfASymlinkedWorkDir(t *testing.T) {
	los34cPinAiJail(t, 0)
	home := isolateHome(t)
	h, _ := mustHandler(t)

	real := filepath.Join(home, "projects", "app")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mcp := filepath.Join(real, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", mcp, err)
	}
	link := filepath.Join(home, "work-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	sb := h.newSandbox(SandboxConfig{Enabled: true, WorkDir: link})

	if !los34fMasked(sb, mcp) {
		t.Fatalf("%s is reachable through the resolved cwd but not masked, masks = %v", mcp, sb.cfg.MaskedPaths)
	}
}

// YM-314 (3): the opencode session database and its SQLite siblings. The daemon
// reads the database unsandboxed, so a run that can write it — or append a WAL
// frame the next read applies — steers what it concludes about agents. The
// siblings are masked while they do not exist, because SQLite deletes them on the
// last close and an existence filter would leave exactly those two names open.
func TestLos34gMasksTheOpencodeSessionDatabase(t *testing.T) {
	dbPath := func(home string) string {
		return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	}
	writeDB := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("SQLite format 3\x00"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	t.Run("existing database", func(t *testing.T) {
		los34cPinAiJail(t, 0)
		home := isolateHome(t)
		unsetEnvForTest(t, "XDG_DATA_HOME")
		unsetEnvForTest(t, "XDG_CONFIG_HOME")
		db := dbPath(home)
		files := []string{db, db + "-wal", db + "-shm", db + "-journal"}
		for _, file := range files {
			writeDB(t, file)
		}
		h, _ := mustHandler(t)

		sb := h.newSandbox(SandboxConfig{Enabled: true})

		for _, file := range files {
			if !los34fMasked(sb, file) {
				t.Errorf("%s is writable from a run but read by the daemon, masks = %v", file, sb.cfg.MaskedPaths)
			}
		}
	})

	t.Run("idle database", func(t *testing.T) {
		los34cPinAiJail(t, 0)
		home := isolateHome(t)
		unsetEnvForTest(t, "XDG_DATA_HOME")
		unsetEnvForTest(t, "XDG_CONFIG_HOME")
		db := dbPath(home)
		writeDB(t, db)
		h, _ := mustHandler(t)

		sb := h.newSandbox(SandboxConfig{Enabled: true})

		// SQLite deletes -wal/-shm on the last close, so they are absent exactly
		// when a run could create them. They are materialised instead of skipped.
		for _, file := range []string{db + "-wal", db + "-shm", db + "-journal"} {
			if !los34fMasked(sb, file) {
				t.Errorf("%s is creatable by a run and not masked, masks = %v", file, sb.cfg.MaskedPaths)
			}
			if _, err := os.Stat(file); err != nil {
				t.Errorf("%s was not materialised as a mask placeholder, stat: %v", file, err)
			}
		}
	})

	t.Run("absent database", func(t *testing.T) {
		los34cPinAiJail(t, 0)
		home := isolateHome(t)
		unsetEnvForTest(t, "XDG_DATA_HOME")
		unsetEnvForTest(t, "XDG_CONFIG_HOME")
		db := dbPath(home)
		h, _ := mustHandler(t)

		sb := h.newSandbox(SandboxConfig{Enabled: true})

		// Nothing to protect with a file mask, and a placeholder for the database
		// would make the daemon start its opencode scanner against a schema that is
		// not there. The directory mask closes the creation case instead: a run that
		// can create the database would poison exactly what the daemon reads.
		if !los34fMasked(sb, filepath.Dir(db)) {
			t.Errorf("the opencode data directory is not masked, so a run can create the database; masks = %v", sb.cfg.MaskedPaths)
		}
		for _, file := range []string{db, db + "-wal", db + "-shm"} {
			if los34fMasked(sb, file) {
				t.Errorf("%s does not exist, a file mask would be a placeholder; masks = %v", file, sb.cfg.MaskedPaths)
			}
			if !los34fCoveredByMask(sb, file) {
				t.Errorf("%s is not covered by any mask, masks = %v", file, sb.cfg.MaskedPaths)
			}
		}
		if _, err := os.Stat(db); !os.IsNotExist(err) {
			t.Errorf("a database that does not exist was materialised, stat err = %v", err)
		}
	})

	t.Run("custom xdg data root", func(t *testing.T) {
		los34cPinAiJail(t, 0)
		home := isolateHome(t)
		unsetEnvForTest(t, "XDG_CONFIG_HOME")
		dataRoot := t.TempDir()
		t.Setenv("XDG_DATA_HOME", dataRoot)
		// The daemon resolves the session database below $HOME no matter what
		// XDG_DATA_HOME says (handler.go:982, daemon.go:295), so the home default has
		// to be masked on a host that keeps its XDG data elsewhere. The effective
		// root is masked as well, because a run can write there too.
		homeDB := dbPath(home)
		writeDB(t, homeDB)
		xdgDB := filepath.Join(dataRoot, "opencode", "opencode.db")
		writeDB(t, xdgDB)
		h, _ := mustHandler(t)

		sb := h.newSandbox(SandboxConfig{Enabled: true})

		for _, file := range []string{homeDB, homeDB + "-wal", xdgDB, xdgDB + "-wal"} {
			if !los34fMasked(sb, file) {
				t.Errorf("%s is read or writable from a run but not masked, masks = %v", file, sb.cfg.MaskedPaths)
			}
		}
	})

	t.Run("unwritable data directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores the directory mode, the placeholder would succeed")
		}
		los34cPinAiJail(t, 0)
		home := isolateHome(t)
		unsetEnvForTest(t, "XDG_DATA_HOME")
		unsetEnvForTest(t, "XDG_CONFIG_HOME")
		dbDir := filepath.Join(home, "foreign", "opencode")
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		db := filepath.Join(dbDir, "opencode.db")
		writeDB(t, db)
		// A data directory the daemon does not own: the WAL placeholder cannot be
		// created. That may cost the run the sibling channel, but not the run (it
		// would refuse every standard launch on such a host).
		if err := os.Chmod(dbDir, 0o555); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { os.Chmod(dbDir, 0o755) })
		homeDir := filepath.Join(home, ".local", "share", "opencode")
		if err := os.MkdirAll(homeDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		h, _ := mustHandler(t)
		h.SetOpencodeDBPath(db)

		sb := h.newSandbox(SandboxConfig{Enabled: true})

		if sb.cfg.PrepareErr != nil {
			t.Fatalf("an unwritable foreign data directory refused the run: %v", sb.cfg.PrepareErr)
		}
		if !los34fMasked(sb, db) {
			t.Errorf("the database itself needs no placeholder and has to stay masked, masks = %v", sb.cfg.MaskedPaths)
		}
		if _, err := os.Stat(db + "-wal"); !os.IsNotExist(err) {
			t.Errorf("the placeholder was created in a directory the daemon does not own, err = %v", err)
		}
	})

	t.Run("configured database path", func(t *testing.T) {
		los34cPinAiJail(t, 0)
		isolateHome(t)
		unsetEnvForTest(t, "XDG_DATA_HOME")
		unsetEnvForTest(t, "XDG_CONFIG_HOME")
		// paths.opencode_db moves the database the daemon reads out of both derived
		// roots (daemon.go:295). The mask follows the path the handler carries.
		db := filepath.Join(t.TempDir(), "elsewhere", "opencode.db")
		writeDB(t, db)
		h, _ := mustHandler(t)
		h.SetOpencodeDBPath(db)

		sb := h.newSandbox(SandboxConfig{Enabled: true})

		for _, file := range []string{db, db + "-wal", db + "-shm"} {
			if !los34fMasked(sb, file) {
				t.Errorf("%s is the database the daemon reads but is not masked, masks = %v", file, sb.cfg.MaskedPaths)
			}
		}
	})
}

// YM-314 (4), the follow-on: the refusal is infrastructure, not the script's
// fault. The auto-correct pass used to report a refused verification run as
// "auto-correct unsuccessful" and threw away a fix it never tried.
func TestLos34gAutoCorrectSkipsARefusedVerificationRun(t *testing.T) {
	neutralizeClaude(t)
	h, s := mustHandler(t)

	body := "#!/bin/bash\nset -eu\nfor f in a b c d e f; do\n  echo processing $f\ndone\nexit 0\n"
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: patchReply("  echo processing $f", "  echo processed $f")})
	h.autoCorrectVerify = func(string, int, SandboxProfile) (int, error) {
		return -1, fmt.Errorf("%w: mask placeholder /nowhere: permission denied", errMaskPrepare)
	}
	h.scheduler = newScheduler(nil, func(_ ScheduledJob) {})
	const project = "/srv/los34g/job"
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "j-mask", Name: "mask-job", Cron: "*/5 * * * *", Enabled: true,
		Mode: "bash", CapName: "mask_cap", ScriptName: "run", AutoCorrect: true,
		Project: project,
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	scripts := `[{"name":"run","kind":"handler","runtime":"bash","lang":"bash","body":` + jsonString(body) + `}]`
	if resp := h.Handle(Request{Method: "save_cap", Params: map[string]any{
		"name": "mask_cap", "description": "mask", "scripts": scripts,
	}}); resp.Error != "" {
		t.Fatalf("seed save_cap: %s", resp.Error)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "j-mask", JobName: "mask-job", CapName: "mask_cap", ScriptName: "run",
		Command: body, Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	h.processBashJobErrors(context.Background())

	msgs, err := s.GetUnreadBroadcasts("los34g-reader", project)
	if err != nil {
		t.Fatalf("unread broadcasts: %v", err)
	}
	var joined string
	for _, msg := range msgs {
		joined += msg.Content + "\n"
	}
	if !strings.Contains(joined, "SKIPPED") {
		t.Errorf("the refusal was not reported as a skipped verification, broadcasts:\n%s", joined)
	}
	if strings.Contains(joined, "auto-correct unsuccessful") {
		t.Errorf("a never-started verification run was reported as a failed fix, broadcasts:\n%s", joined)
	}
	caps, err := s.GetActiveLearnings("cap", "yesmem", "", "", 0)
	if err != nil {
		t.Fatalf("read caps: %v", err)
	}
	for _, l := range caps {
		meta, metaErr := ParseCapMeta(l.Context)
		if metaErr != nil || meta.Name != "mask_cap" {
			continue
		}
		if meta.Version > 1 {
			t.Errorf("the cap was rewritten from an untested fix, version = %d", meta.Version)
		}
	}
}

// YM-314 (3) against the real binary: with no database yet, a run could create one
// and the daemon would then read it — session rows the DONE-guard evidence layer
// trusts. The data directory is masked as a whole, so nothing a run writes there
// reaches the host.
func TestLos34gRealRunCannotCreateTheSessionDatabase(t *testing.T) {
	requireAiJailPlatform(t)
	home := isolateHome(t)
	dataDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db := filepath.Join(dataDir, "opencode.db")
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
	control := filepath.Join(dataDir, "probe.txt")
	if out, code, err := sb.RunWithProfile(fmt.Sprintf("printf x > %s", los34dShellQuote(control)), 30, ProfileStandard); err != nil || code != 0 {
		t.Fatalf("control write failed (%v, exit=%d, out=%q) — the probe cannot tell a mask from an unreachable path", err, code, out)
	}

	sb.RunWithProfile(fmt.Sprintf("printf 'forged' > %s; printf 'x' > %s", los34dShellQuote(db), los34dShellQuote(db+"-wal")), 30, ProfileStandard)
	for _, file := range []string{db, db + "-wal"} {
		if _, err := os.Stat(file); err == nil {
			t.Errorf("the run created %s on the host — the daemon would read it", file)
		}
	}
}

// YM-314 (4): without HOME the default mask list came back empty and the run
// started anyway — with no secret masked at all, while the same run behind an
// existing home is refused when a mask cannot be applied. The missing home is
// what makes the list empty, so it has to refuse the same way.
func TestLos34gRefusesARunWithoutAHomeDirectory(t *testing.T) {
	los34fPinExecAiJail(t, 0)
	h, _ := mustHandler(t)
	unsetEnvForTest(t, "HOME")

	sb := h.newSandbox(SandboxConfig{Enabled: true})

	if sb.cfg.PrepareErr == nil {
		t.Fatal("no home directory, no masks, and the config carries no refusal")
	}
	out, code, err := sb.RunWithProfile("echo RAN", 10, ProfileStandard)
	if err == nil {
		t.Fatalf("the run was not refused, exit=%d out=%q", code, out)
	}
	if strings.Contains(out, "RAN") {
		t.Fatalf("the command ran without a single mask, out=%q", out)
	}
	if !errors.Is(err, errMaskPrepare) {
		t.Fatalf("error %q does not carry errMaskPrepare, the auto-correct pass would treat it as a script failure", err)
	}
}

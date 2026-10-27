package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// los34cPinAiJail resolves a stand-in ai-jail with a fixed major version, so both
// flag sets render without the real binary. The stand-in drops ai-jail's flags and
// runs the command they precede.
func los34cPinAiJail(t *testing.T, major int) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "ai-jail")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ] && [ \"$1\" != \"bash\" ]; do shift; done\nshift\nexec bash \"$@\"\n"
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

// los34cInside reports whether path sits below dir.
func los34cInside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// YM-276: with ai-jail 0.9.x the standard profile leaves the real home writable,
// so the data dir was readable and writable from a job — agent-*.sock.inject
// (key injection into unsandboxed agent TUIs), yesmem.db, config.yaml. A mask on
// single files cannot cover the agent sockets created later, so the dir as a
// whole has to be masked. Both flag sets, because the mask is the one enforcement
// point the two ai-jail majors share.
func TestJobSandboxMasksTheWholeDataDir(t *testing.T) {
	for _, major := range []int{0, 2} {
		t.Run(fmt.Sprintf("ai-jail-%dx", major), func(t *testing.T) {
			los34cPinAiJail(t, major)
			isolateHome(t)
			h, _ := mustHandler(t)

			sb := h.newSandbox(SandboxConfig{Enabled: true})

			masked := false
			for _, path := range sb.cfg.MaskedPaths {
				if path == h.dataDir {
					masked = true
				}
			}
			if !masked {
				t.Fatalf("the data dir must be masked as a whole, masked = %v", sb.cfg.MaskedPaths)
			}

			flags := strings.Join(sb.sandboxFlags(ProfileStandard), " ")
			if !strings.Contains(flags, "--mask "+h.dataDir) {
				t.Fatalf("standard must render the data dir mask, flags = %v", flags)
			}
		})
	}
}

// YM-276: the mask only holds if nothing the run needs is mounted back out of the
// masked dir. A read-only or read-write mount below it re-exposes exactly the part
// the mask is meant to hide, so such a mount is dropped.
func TestJobSandboxMountsNothingFromTheDataDir(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)

	// The bash path is the one that adds a read-write mount (the job log dir), so
	// take its config: otherwise the rw half of the invariant never sees a mount.
	jobCfg := h.bashSandboxConfig(&ScheduledJob{ID: "sched-los34c-mount", Name: "los34c"}, nil, ProfileStandard)
	if len(jobCfg.RWMapPaths) == 0 {
		t.Fatal("the bash job config carries no job dir mount, the rw check would be vacuous")
	}
	// A configured mount that points into the data dir must not survive.
	inside := SandboxConfig{
		Enabled:        true,
		ReadOnlyMounts: []string{filepath.Join(h.dataDir, "yesmem.db")},
	}

	cases := map[string]SandboxConfig{
		"cap":       {Enabled: true},
		"bash job":  jobCfg,
		"config-in": inside,
	}
	for name, cfg := range cases {
		sb := h.newSandbox(cfg)
		mounts := append([]string{}, sb.cfg.ReadOnlyMounts...)
		mounts = append(mounts, sb.cfg.RWMapPaths...)
		for _, mount := range mounts {
			if los34cInside(h.dataDir, mount) {
				t.Errorf("%s: mount %q lives inside the masked data dir %q", name, mount, h.dataDir)
			}
		}
	}
}

// YM-276: the restricted job socket used to sit in the data dir, where the mask
// would hide it from the very runs that need it. It moves to a sibling directory.
func TestJobSocketLivesOutsideTheDataDir(t *testing.T) {
	h, _ := mustHandler(t)

	sock := JobSocketPath(h.dataDir)
	if los34cInside(h.dataDir, sock) {
		t.Fatalf("job socket %q lives inside the masked data dir %q", sock, h.dataDir)
	}
	// A sibling, not an unrelated location: the runtime dir shares the data dir's
	// parent, so an install that moves one moves both.
	if got, want := filepath.Dir(filepath.Dir(sock)), filepath.Dir(h.dataDir); got != want {
		t.Errorf("job socket %q is not one directory beside %q (parent = %q)", sock, h.dataDir, got)
	}
}

// YM-276: the job log dirs are mapped read-write into the sandbox. Under the data
// dir they would poke a hole into the mask.
func TestJobDirLivesOutsideTheDataDir(t *testing.T) {
	h, _ := mustHandler(t)

	dir, err := h.ensureJobDir(&ScheduledJob{ID: "sched-los34c-1", Name: "los34c"})
	if err != nil {
		t.Fatalf("ensureJobDir: %v", err)
	}
	if los34cInside(h.dataDir, dir) {
		t.Fatalf("job dir %q lives inside the masked data dir %q", dir, h.dataDir)
	}
	info, statErr := os.Stat(dir)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("job dir not created: %v", statErr)
	}
}

// The daemon listens on the job socket before any job exists, so the runtime dir
// has to be creatable on its own.
func TestEnsureRuntimeDirCreatesTheSiblingDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	dir, err := EnsureRuntimeDir(dataDir)
	if err != nil {
		t.Fatalf("EnsureRuntimeDir: %v", err)
	}
	if got, want := filepath.Dir(dir), filepath.Dir(dataDir); got != want {
		t.Errorf("runtime dir %q is not a sibling of %q", dir, dataDir)
	}
	info, statErr := os.Stat(dir)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("runtime dir not created: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %o, want 700 (the restricted socket lives there)", perm)
	}

	if _, err := EnsureRuntimeDir(""); err == nil {
		t.Error("an empty data dir must be refused, not turned into a relative path")
	}
}

// The directory mode is the only access control on the restricted socket, and
// MkdirAll neither repairs an existing mode nor refuses a symlink. A sandboxed job
// on 0.9.x can write in ~/.claude, so both cases are reachable (YM-276 review).
func TestEnsureRuntimeDirRefusesASymlinkAndRepairsTheMode(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "yesmem")
	runtimeDir := RuntimeDir(dataDir)

	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	if _, err := EnsureRuntimeDir(dataDir); err != nil {
		t.Fatalf("EnsureRuntimeDir on an existing dir: %v", err)
	}
	info, err := os.Stat(runtimeDir)
	if err != nil {
		t.Fatalf("stat runtime dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %o after EnsureRuntimeDir, want 700", perm)
	}

	// A symlink at the runtime path would move the socket and the job dirs to its
	// target; it has to be refused, not followed.
	target := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.RemoveAll(runtimeDir); err != nil {
		t.Fatalf("remove runtime dir: %v", err)
	}
	if err := os.Symlink(target, runtimeDir); err != nil {
		t.Fatalf("symlink runtime dir: %v", err)
	}
	if _, err := EnsureRuntimeDir(dataDir); err == nil {
		t.Fatal("a symlinked runtime dir must be refused")
	}
}

// YM-276 against the real binary: inside the sandbox the data dir is empty, while
// the runtime dir beside it stays readable. 0.9.x is the case that matters (it has
// no private home), and that is the version this host carries.
//
// The probe directories live under ~/.claude: ai-jail's 0.9.x standard profile
// does not mount the whole home, it builds a synthetic home and binds selected
// dotdirs into it, so a fresh directory directly under $HOME is invisible while a
// sibling of the data dir is reachable. /tmp is a throwaway tmpfs as well.
func TestRunWithProfile_DataDirMaskHidesTheDataDirForReal(t *testing.T) {
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		t.Skipf("no home directory: %v", homeErr)
	}
	root, err := os.MkdirTemp(filepath.Join(home, ".claude"), "los34c-datadir-")
	if err != nil {
		t.Skipf("no writable ~/.claude: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	dataDir := filepath.Join(root, "yesmem")
	runtimeDir := filepath.Join(root, "yesmem-run")
	for _, dir := range []string{dataDir, runtimeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataDir, "yesmem.db"), []byte("memory"), 0o600); err != nil {
		t.Fatalf("write decoy db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "job.sock"), []byte("socket"), 0o600); err != nil {
		t.Fatalf("write runtime file: %v", err)
	}

	// The config comes from the handler, not from the test: that is what ships.
	isolateHome(t)
	h, _ := mustHandler(t)
	h.dataDir = dataDir
	s := realAiJailSandbox(t, h.newSandbox(SandboxConfig{Enabled: true}).cfg)

	probe := `printf 'DATADIR_COUNT=%s\n' "$(ls -A ` + dataDir + ` 2>/dev/null | wc -l)"; ` +
		`printf 'RUNTIME_FILE=%s\n' "$(cat ` + filepath.Join(runtimeDir, "job.sock") + ` 2>/dev/null || echo missing)"`
	out, code, err := s.RunWithProfile(probe, 30, ProfileStandard)
	if err != nil {
		t.Fatalf("sandboxed run: %v (out=%q)", err, out)
	}
	if code != 0 {
		t.Fatalf("probe exit = %d (out=%q)", code, out)
	}
	if got := probeValue(t, out, "DATADIR_COUNT"); got != "0" {
		t.Errorf("data dir still shows %q entries inside the sandbox, want 0", got)
	}
	if got := probeValue(t, out, "RUNTIME_FILE"); got != "socket" {
		t.Errorf("runtime dir is not readable inside the sandbox (%q)", got)
	}
}

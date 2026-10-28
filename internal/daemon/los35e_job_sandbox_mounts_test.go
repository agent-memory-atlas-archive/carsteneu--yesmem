package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// YM-353 (OPS-87, key 2): under ai-jail 2.x a run sees its own mounts and nothing
// else, so a standard bash job that calls store() needs the yesmem CLI and the
// restricted job socket mapped in. bashSandboxConfig deliberately leaves
// ReadOnlyMounts nil and lets newSandbox fill the defaults in; this pins that
// invariant for the job path, because a non-nil but incomplete list would silence
// the default for every standard job without any test noticing.
func TestLos35eStandardBashJobMountsCLIAndJobSocket(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)

	// PATH pins the CLI lookup, so the expected mount is the path this test planted
	// and not whatever the developer happens to have installed.
	bin := t.TempDir()
	cli := filepath.Join(bin, "yesmem")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stand-in cli: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	job := &ScheduledJob{ID: "sched-los35e-mount", Name: "los35e-mount"}
	jobCfg := h.bashSandboxConfig(job, nil, ProfileStandard)
	if jobCfg.ReadOnlyMounts != nil {
		t.Fatalf("the job config must leave ReadOnlyMounts nil so newSandbox fills the defaults, got %v", jobCfg.ReadOnlyMounts)
	}
	sb := h.newSandbox(jobCfg)

	sock := JobSocketPath(h.dataDir)
	for _, want := range []string{cli, sock} {
		if !los35eContainsPath(sb.cfg.ReadOnlyMounts, want) {
			t.Fatalf("standard bash job does not mount %q, mounts = %v", want, sb.cfg.ReadOnlyMounts)
		}
	}

	// The mount list is only worth something if the profile renders it.
	flags := strings.Join(sb.sandboxFlags(ProfileStandard), " ")
	for _, want := range []string{cli, sock} {
		if !strings.Contains(flags, "--map "+want) {
			t.Errorf("standard flags do not map %q: %s", want, flags)
		}
	}

	// And the run has to be pointed at that socket, or the mount is decorative.
	if !los35eContainsEnv(sb.cfg.Env, "YESMEM_SOCK="+sock) {
		t.Errorf("the job sandbox does not point YESMEM_SOCK at the restricted socket: %v", sb.cfg.Env)
	}
}

func los35eContainsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

func los35eContainsEnv(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

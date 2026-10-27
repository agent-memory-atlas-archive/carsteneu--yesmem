package daemon

import (
	"os"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-267 (5): the auto-correct pass ran the corrected script in a sandbox built
// from a bare literal: work dir yes, job dir no. The verification then differed
// from the run it was verifying — a fix whose log write needs YESMEM_JOB_DIR
// failed verification, and one that only works because it silently logs into the
// sandbox's throwaway /tmp passed it while still writing nowhere in production.
func TestAutoCorrectVerificationSeesTheJobDir(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)

	job := &ScheduledJob{ID: "sched-los34b-9", Name: "probe", AllowedPorts: "443", Sandbox: ProfileStandard}
	cfg := h.autoCorrectSandboxConfig(
		storage.BashJobRun{JobID: "sched-los34b-9", JobName: "probe"},
		job, "443", []string{"example.com"}, ProfileStandard,
	)

	if len(cfg.RWMapPaths) != 1 {
		t.Fatalf("verification run must mount the job dir, got %v", cfg.RWMapPaths)
	}
	if _, err := os.Stat(cfg.RWMapPaths[0]); err != nil {
		t.Fatalf("job dir %q does not exist: %v", cfg.RWMapPaths[0], err)
	}
	env := strings.Join(cfg.Env, " ")
	if !strings.Contains(env, "YESMEM_JOB_DIR="+cfg.RWMapPaths[0]) {
		t.Fatalf("verification run must get YESMEM_JOB_DIR for the same dir, env = %v", cfg.Env)
	}
	if cfg.WorkDir == "" {
		t.Fatal("verification run must keep a work dir")
	}
}

// Under a locked-down profile nothing can be mounted, so the job dir has to stay
// away there rather than becoming a path the cap cannot reach.
func TestAutoCorrectVerificationSkipsTheJobDirUnderStrict(t *testing.T) {
	h, _ := mustHandler(t)
	isolateHome(t)

	job := &ScheduledJob{ID: "sched-los34b-10", Name: "strictprobe", Sandbox: ProfileStrict}
	cfg := h.autoCorrectSandboxConfig(
		storage.BashJobRun{JobID: "sched-los34b-10", JobName: "strictprobe"},
		job, "443", nil, ProfileStrict,
	)

	if len(cfg.RWMapPaths) != 0 {
		t.Fatalf("strict mounts nothing: rw=%v", cfg.RWMapPaths)
	}
	if _, ok := los32cEnvValue(cfg.Env, "YESMEM_JOB_DIR"); ok {
		t.Fatalf("strict cannot reach the job dir: env=%v", cfg.Env)
	}
}

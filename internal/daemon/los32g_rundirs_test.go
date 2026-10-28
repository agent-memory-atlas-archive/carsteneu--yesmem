package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// los32gAge makes a run directory look older than the sweep's grace.
func los32gAge(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-2 * runDirMinAge)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("age %s: %v", dir, err)
	}
}

// YM-295 (5): the run work dirs of project-less jobs and of agents were created
// and never removed. The sweep drops the ones whose key has no row any more and
// keeps every key that is still a job or an agent, because that directory is the
// cwd of a live process.
func TestLos32gPruneRunDirsDropsOrphansAndKeepsLiveOnes(t *testing.T) {
	isolateHome(t)
	h, s := los32dHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-los32g-live", Project: "los32g-nowhere", Section: "los32g-live",
		SessionID: "ses-los32g-live", Status: "stopped", Backend: "codex",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	if err := s.SaveScheduledJob(storage.ScheduledJobRow{
		ID: "sched-los32g-live", Name: "sched-los32g-live", Mode: "bash", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveScheduledJob: %v", err)
	}

	liveJob, err := EnsureRunWorkDir(h.dataDir, "sched-los32g-live")
	if err != nil {
		t.Fatalf("run dir for the live job: %v", err)
	}
	liveAgent, err := EnsureRunWorkDir(h.dataDir, "agent-los32g-live")
	if err != nil {
		t.Fatalf("run dir for the live agent: %v", err)
	}
	orphan, err := EnsureRunWorkDir(h.dataDir, "sched-los32g-gone")
	if err != nil {
		t.Fatalf("run dir for the orphaned job: %v", err)
	}
	orphanLog := filepath.Join(RuntimeDir(h.dataDir), "jobs", jobDirName("sched-los32g-gone"))
	if err := os.MkdirAll(orphanLog, 0o700); err != nil {
		t.Fatalf("log dir for the orphaned job: %v", err)
	}
	// A run that ended seconds ago may belong to a job deleted while it ran; the
	// sweep waits out the grace before it removes a directory.
	young, err := EnsureRunWorkDir(h.dataDir, "sched-los32g-young")
	if err != nil {
		t.Fatalf("run dir for the young orphan: %v", err)
	}
	old := time.Now().Add(-2 * runDirMinAge)
	for _, dir := range []string{orphan, orphanLog} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatalf("age %s: %v", dir, err)
		}
	}

	h.pruneRunDirs()

	for _, dir := range []string{orphan, orphanLog} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("orphaned directory survived the sweep: %s (%v)", dir, err)
		}
	}
	for _, dir := range []string{liveJob, liveAgent, young} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the sweep removed a directory it must keep: %s (%v)", dir, err)
		}
	}
}

// A cap run's directory is keyed on "cap:<name>" and has no row in any table: it
// is the cap's workspace between runs, so the sweep leaves that key family alone.
func TestLos32gPruneRunDirsKeepsCapWorkspaces(t *testing.T) {
	isolateHome(t)
	h, _ := los32dHandler(t)
	dir, err := EnsureRunWorkDir(h.dataDir, "cap:some_cap")
	if err != nil {
		t.Fatalf("cap work dir: %v", err)
	}
	los32gAge(t, dir)

	h.pruneRunDirs()

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the sweep removed a cap's workspace: %v", err)
	}
}

// A job deleted while its run is still in flight loses its row, not its cwd.
func TestLos32gPruneRunDirsKeepsARunInFlight(t *testing.T) {
	isolateHome(t)
	h, _ := los32dHandler(t)
	dir, err := EnsureRunWorkDir(h.dataDir, "sched-los32g-inflight")
	if err != nil {
		t.Fatalf("run dir: %v", err)
	}
	los32gAge(t, dir)

	h.runStarting("sched-los32g-inflight")
	h.pruneRunDirs()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the sweep removed the cwd of a run in flight: %v", err)
	}

	h.runFinished("sched-los32g-inflight")
	h.pruneRunDirs()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory of a finished, deleted run survived: %v", err)
	}
}

// A scheduler agent's cwd is keyed on the job that spawned it, so the agent row is
// the only thing that names the directory after the job row is gone.
func TestLos32gPruneRunDirsKeepsASchedulerAgentsCwd(t *testing.T) {
	isolateHome(t)
	h, s := los32dHandler(t)
	dir, err := EnsureRunWorkDir(h.dataDir, "sched-los32g-agentjob")
	if err != nil {
		t.Fatalf("run dir: %v", err)
	}
	los32gAge(t, dir)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-los32g-fromjob", Project: dir, Section: "los32g-fromjob",
		SessionID: "ses-los32g-fromjob", Status: "stopped", Backend: "claude", WorkDir: dir,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	h.pruneRunDirs()

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the sweep removed the cwd an agent row still points at: %v", err)
	}
}

// The sweep base is server state a run can reach, so a symlink there must not
// turn RemoveAll into a delete outside the runtime dir.
func TestLos32gPruneRunDirsRefusesASymlinkedBase(t *testing.T) {
	isolateHome(t)
	h, _ := los32dHandler(t)

	outside := t.TempDir()
	victim := filepath.Join(outside, jobDirName("sched-los32g-victim"))
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	old := time.Now().Add(-2 * runDirMinAge)
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatalf("age victim: %v", err)
	}
	base := runWorkDirBase(h.dataDir)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	if err := os.Symlink(outside, base); err != nil {
		t.Fatalf("symlink base: %v", err)
	}

	h.pruneRunDirs()

	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the sweep followed a symlinked base and removed %s: %v", victim, err)
	}
}

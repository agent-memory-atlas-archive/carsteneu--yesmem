package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los32dSession registers a session the way the indexer does (absolute project).
func los32dSession(t *testing.T, s *storage.Store, id, project string) {
	t.Helper()
	if err := s.UpsertSession(&models.Session{ID: id, Project: project, StartedAt: time.Now()}); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

// los32dJobProject is the absolute project a job is created from. The bare
// basename of it is what used to be broadcast, and the basename matches every
// checkout of that name.
const los32dJobProject = "/srv/los32d/memory/yesmem"

// seedLos32dBashErrorJob schedules a job the way schedule create does (project is
// the creating caller's directory) and queues one failed run for it.
func seedLos32dBashErrorJob(t *testing.T, h *Handler, s *storage.Store, jobID, project string) {
	t.Helper()
	h.scheduler = newScheduler(nil, nil)
	if err := h.scheduler.AddJob(ScheduledJob{ID: jobID, Name: jobID, Project: project, IntervalSeconds: 300}); err != nil {
		t.Fatalf("add job %s: %v", jobID, err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID:    jobID,
		JobName:  jobID,
		Command:  "exit 1",
		Status:   "error",
		ExitCode: 1,
		Output:   "boom",
		ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run for %s: %v", jobID, err)
	}
}

func los32dBroadcasts(t *testing.T, s *storage.Store, reader, project string) []string {
	t.Helper()
	msgs, err := s.GetUnreadBroadcasts(reader, project)
	if err != nil {
		t.Fatalf("get broadcasts for %q: %v", project, err)
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

// los32dHandler returns a handler whose data dir is a real subdirectory, so the
// runtime dir beside it lands inside the test's temp tree and not next to it.
func los32dHandler(t *testing.T) (*Handler, *storage.Store) {
	t.Helper()
	h, s := mustHandler(t)
	h.dataDir = filepath.Join(t.TempDir(), "data")
	return h, s
}

// YM-286 (1): a failing bash job is announced to the project the job was created
// from. The old address was the bare basename "yesmem", which GetUnreadBroadcasts
// also matches for every checkout of that name.
func TestLos32dBashErrorBroadcastUsesTheJobsProject(t *testing.T) {
	neutralizeClaude(t)
	isolateHome(t)
	h, s := los32dHandler(t)
	seedLos32dBashErrorJob(t, h, s, "sched-los32d-proj", los32dJobProject)
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: "diagnosis text"})

	h.processBashJobErrors(context.Background())

	if got := los32dBroadcasts(t, s, "reader-elsewhere", los32dJobProject); len(got) != 1 {
		t.Fatalf("the job's own project must receive the broadcast, got %d: %v", len(got), got)
	}
	if got := los32dBroadcasts(t, s, "reader-elsewhere", "yesmem"); len(got) != 0 {
		t.Fatalf("a bare-basename reader must not receive it (over-delivery), got %d: %v", len(got), got)
	}
}

// Without a recorded project nothing is invented. The basename address reached
// every checkout of that name, and an empty project is unaddressable, so no row
// is written at all; the run is still marked processed and the daemon log keeps
// the message.
func TestLos32dBashErrorBroadcastInventsNoProject(t *testing.T) {
	neutralizeClaude(t)
	isolateHome(t)
	h, s := los32dHandler(t)
	seedLos32dBashErrorJob(t, h, s, "sched-los32d-noproj", "")
	h.SetAutoCorrectClient(&autoCorrectStubClient{reply: "diagnosis text"})

	h.processBashJobErrors(context.Background())

	for _, reader := range []string{"yesmem", ""} {
		if got := los32dBroadcasts(t, s, "reader-any", reader); len(got) != 0 {
			t.Errorf("project %q received a broadcast for a job without a project: %v", reader, got)
		}
	}
	if runs, err := s.GetUnprocessedBashErrors(10); err != nil {
		t.Fatalf("get unprocessed: %v", err)
	} else if len(runs) != 0 {
		t.Errorf("the run must still be marked processed, got %d unprocessed", len(runs))
	}
}

// YM-286 (2): an absolute project is its own working directory. Joining it onto
// $HOME/projects built a nested path that belongs to no repository, and created it.
func TestLos32dResolveAgentWorkDirDoesNotInventANestedPath(t *testing.T) {
	home := isolateHome(t)
	h, _ := los32dHandler(t)

	got := h.resolveAgentWorkDir(los32dJobProject, "")

	if got != los32dJobProject {
		t.Fatalf("resolveAgentWorkDir(%q) = %q, want the project path", los32dJobProject, got)
	}
	if _, err := os.Stat(filepath.Join(home, "projects")); !os.IsNotExist(err) {
		t.Errorf("%s/projects was created for a project that names its own path", home)
	}
}

// The job's working directory is the project it was created from (YM-281), not
// the daemon's own name resolved against $HOME/projects.
func TestLos32dJobWorkDirIsTheJobsProject(t *testing.T) {
	home := isolateHome(t)
	h, _ := los32dHandler(t)
	project := t.TempDir()
	job := &ScheduledJob{ID: "sched-los32d-wd", Name: "los32d-wd", Project: project}

	if got := h.bashSandboxConfig(job, nil, ProfileStandard).WorkDir; got != project {
		t.Fatalf("job WorkDir = %q, want %q", got, project)
	}
	if _, err := os.Stat(filepath.Join(home, "projects", "yesmem")); !os.IsNotExist(err) {
		t.Errorf("$HOME/projects/yesmem was created for a job that names its project")
	}
}

// A job created before jobs carried their project (YM-281) has none to name. Its
// run still needs a working directory, but not one that pretends to be a checkout.
func TestLos32dJobWorkDirWithoutAProjectInventsNoCheckout(t *testing.T) {
	home := isolateHome(t)
	h, _ := los32dHandler(t)
	job := &ScheduledJob{ID: "sched-los32d-nowd", Name: "los32d-nowd"}

	got := h.bashSandboxConfig(job, nil, ProfileStandard).WorkDir

	if got == "" {
		t.Fatal("a run still needs a working directory")
	}
	if got == filepath.Join(home, "projects", "yesmem") {
		t.Fatalf("WorkDir = %q: the daemon's own name resolved against $HOME/projects", got)
	}
	if _, err := os.Stat(filepath.Join(home, "projects")); !os.IsNotExist(err) {
		t.Errorf("%s/projects was created for a job without a project", home)
	}
	if !strings.HasPrefix(got, RuntimeDir(h.dataDir)+string(filepath.Separator)) {
		t.Errorf("WorkDir = %q, want a directory inside %q", got, RuntimeDir(h.dataDir))
	}
}

// A project that has vanished (deleted worktree) is not a working directory: the
// run would silently inherit the daemon's cwd instead, and the headless path would
// recreate the path it is supposed to run in.
func TestLos32dJobWorkDirFallsBackWhenTheProjectIsGone(t *testing.T) {
	home := isolateHome(t)
	h, _ := los32dHandler(t)
	gone := filepath.Join(t.TempDir(), "deleted-worktree")

	got := h.jobWorkDir(&ScheduledJob{ID: "sched-los32d-gone", Name: "los32d-gone", Project: gone})

	if got == gone {
		t.Fatalf("jobWorkDir = %q, want the run work dir for a vanished project", got)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("the vanished project path %q was recreated", gone)
	}
	if _, err := os.Stat(filepath.Join(home, "projects")); !os.IsNotExist(err) {
		t.Errorf("%s/projects was created for a vanished project", home)
	}
}

// An ambiguous short name must not be resolved by inventing a third directory:
// that is how $HOME/projects/yesmem came to exist next to the two real checkouts.
func TestLos32dResolveAgentWorkDirRefusesAnAmbiguousShortName(t *testing.T) {
	home := isolateHome(t)
	h, s := los32dHandler(t)
	for id, project := range map[string]string{
		"ses-los32d-amb-a": "/srv/los32d/a/yesmem",
		"ses-los32d-amb-b": "/srv/los32d/b/yesmem",
	} {
		los32dSession(t, s, id, project)
	}

	got := h.resolveAgentWorkDir("yesmem", "")

	if got != "" {
		t.Fatalf("resolveAgentWorkDir(ambiguous) = %q, want \"\"", got)
	}
	if _, err := os.Stat(filepath.Join(home, "projects")); !os.IsNotExist(err) {
		t.Errorf("%s/projects was created for an ambiguous short name", home)
	}
}

// Without an address nothing may rewrite a cap body either: an auto-correct
// applies model-authored code inside a sandbox and announces the result only by
// broadcast, so a job without a project would mutate a live cap silently.
func TestLos32dBashErrorAutoCorrectNeedsAnAddress(t *testing.T) {
	neutralizeClaude(t)
	isolateHome(t)
	h, s := los32dHandler(t)
	h.scheduler = newScheduler(nil, nil)
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "sched-los32d-noaddr", Name: "los32d-noaddr", IntervalSeconds: 300,
		CapName: "los32d_addr", AutoCorrect: true,
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "sched-los32d-noaddr", JobName: "los32d-noaddr", CapName: "los32d_addr",
		Command: "exit 1", Status: "error", ExitCode: 1, ErrorMsg: "exit status 1",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	stub := &autoCorrectStubClient{reply: "echo fixed\nexit 0\n"}
	h.SetAutoCorrectClient(stub)

	h.processBashJobErrors(context.Background())

	if got := stub.callCount(); got != 0 {
		t.Errorf("auto-correct must not run without an address, got %d model calls", got)
	}
	if runs, err := s.GetUnprocessedBashErrors(10); err != nil {
		t.Fatalf("get unprocessed: %v", err)
	} else if len(runs) != 0 {
		t.Errorf("the run must still be marked processed, got %d unprocessed", len(runs))
	}
}

// The project column is caller supplied; the filesystem root is never a working
// directory: a 2.x run would get the whole filesystem writable.
func TestLos32dJobWorkDirRefusesTheFilesystemRoot(t *testing.T) {
	isolateHome(t)
	h, _ := los32dHandler(t)
	job := &ScheduledJob{ID: "sched-los32d-root", Name: "los32d-root", Project: "/"}

	got := h.jobWorkDir(job)

	if got == "/" || got == "" {
		t.Fatalf("jobWorkDir(root) = %q, want a usable directory", got)
	}
}

// A symlink at the run work dir would move the writable cwd of every
// project-less run to its target; it has to be refused, not followed.
func TestLos32dEnsureRunWorkDirRefusesASymlink(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	runtimeDir, err := EnsureRuntimeDir(dataDir)
	if err != nil {
		t.Fatalf("ensure runtime dir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(runtimeDir, runWorkDirName)); err != nil {
		t.Fatalf("symlink run work dir: %v", err)
	}

	if _, err := EnsureRunWorkDir(dataDir, "sched-los32d-sym"); err == nil {
		t.Fatal("a symlinked run work dir must be refused")
	}
}

// A repository can ship .claude as a symlink; the spawn must not write through it.
// The writer that had to refuse this is gone (YM-300): a spawn leaves the
// repository alone, so the guard is now the absence of the write itself. The
// dangling-file shape is covered by the same absence.
func TestLos32dSpawnWritesNoPermissionFileThroughASymlinkedClaudeDir(t *testing.T) {
	h, _ := mustHandler(t)
	h.disableAgentProcesses = false
	repo := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(repo, ".claude")); err != nil {
		t.Fatalf("symlink .claude: %v", err)
	}
	captured := los57eCaptureSpawnArgs(t)

	resp := h.handleSpawnAgent(map[string]any{
		"project":  repo,
		"section":  "los32d-symlinked-claude",
		"work_dir": repo,
		"backend":  "claude",
	})
	if resp.Error != "" {
		t.Fatalf("spawn_agent: %s", resp.Error)
	}

	if _, err := os.Stat(filepath.Join(target, "settings.local.json")); !os.IsNotExist(err) {
		t.Fatalf("the permission file was written through the symlink into %s", target)
	}
	select {
	case <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("the stubbed bridge was never reached")
	}
}

// A short name that resolves to nothing has no directory. Inventing
// $HOME/projects/<name> put a checkout next to the real ones that nobody reads.
func TestLos32dResolveAgentWorkDirInventsNothingForAnUnknownShortName(t *testing.T) {
	home := isolateHome(t)
	h, _ := los32dHandler(t)

	got := h.resolveAgentWorkDir("los32d-nowhere", "")

	if got != "" {
		t.Fatalf("resolveAgentWorkDir(unknown) = %q, want \"\"", got)
	}
	if _, err := os.Stat(filepath.Join(home, "projects")); !os.IsNotExist(err) {
		t.Errorf("%s/projects was created for an unknown short name", home)
	}
}

// The daemon's cwd is not the agent's project: a spawn that cannot name a
// directory fails instead of starting somewhere arbitrary.
func TestLos32dSpawnAgentWithoutAWorkDirFails(t *testing.T) {
	isolateHome(t)
	h, s := los32dHandler(t)

	resp := h.handleSpawnAgent(map[string]any{
		"project": "los32d-nowhere",
		"section": "los32d-no-workdir",
		"backend": "claude",
	})

	if resp.Error == "" {
		t.Fatal("a spawn without a resolvable work dir must fail")
	}
	if !strings.Contains(resp.Error, "no working directory") {
		t.Fatalf("error = %q, want it to name the missing work dir", resp.Error)
	}
	if agents, err := s.AgentList(""); err != nil {
		t.Fatalf("list agents: %v", err)
	} else if len(agents) != 0 {
		t.Fatalf("no agent row may be created, got %d", len(agents))
	}
}

// A run that failed before its command ran (a vanished work dir, an unresolvable
// cap) carries no command; asking a model to "fix" that would rewrite a live cap
// body over an infrastructure error.
func TestLos32dBashErrorSkipsARunWithoutACommand(t *testing.T) {
	neutralizeClaude(t)
	isolateHome(t)
	h, s := los32dHandler(t)
	h.scheduler = newScheduler(nil, nil)
	if err := h.scheduler.AddJob(ScheduledJob{
		ID: "sched-los32d-nocmd", Name: "los32d-nocmd", IntervalSeconds: 300,
		Project: los32dJobProject, CapName: "los32d_nocmd", AutoCorrect: true,
	}); err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SaveBashJobRun(storage.BashJobRun{
		JobID: "sched-los32d-nocmd", JobName: "los32d-nocmd", CapName: "los32d_nocmd",
		Status: "error", ExitCode: -1, ErrorMsg: "no working directory for job",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	stub := &autoCorrectStubClient{reply: "echo fixed\nexit 0\n"}
	h.SetAutoCorrectClient(stub)

	h.processBashJobErrors(context.Background())

	if got := stub.callCount(); got != 0 {
		t.Errorf("a run without a command must not reach the model, got %d calls", got)
	}
	if runs, err := s.GetUnprocessedBashErrors(10); err != nil {
		t.Fatalf("get unprocessed: %v", err)
	} else if len(runs) != 0 {
		t.Errorf("the run must still be marked processed, got %d unprocessed", len(runs))
	}
}

// The user's home is the one cwd ai-jail 2.x must not get: it replaces the
// sandbox's private home with a write bind of the real one.
func TestLos32dJobWorkDirRefusesTheHomeDirectory(t *testing.T) {
	home := isolateHome(t)
	h, _ := los32dHandler(t)
	job := &ScheduledJob{ID: "sched-los32d-home", Name: "los32d-home", Project: home}

	got := h.jobWorkDir(job)

	if got == home {
		t.Fatalf("jobWorkDir(home) = %q, want the run work dir", got)
	}
	if !strings.HasPrefix(got, RuntimeDir(h.dataDir)+string(filepath.Separator)) {
		t.Errorf("jobWorkDir(home) = %q, want a directory inside %q", got, RuntimeDir(h.dataDir))
	}
}

// A resume that lands in the daemon-owned run dir must record that directory: the
// session bind narrows by work_dir, and a row claiming "" cannot be told apart
// from the other checkouts of its project.
func TestLos32dResumePersistsTheRunWorkDirItChose(t *testing.T) {
	isolateHome(t)
	h, s := los32dHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-los32d-resume", Project: "los32d-nowhere", Section: "los32d-resume",
		SessionID: "ses-los32d-resume", Status: "stopped", Backend: "codex",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.handleResumeAgent(map[string]any{"to": "agent-los32d-resume"})
	if resp.Error != "" {
		t.Fatalf("resume: %s", resp.Error)
	}

	got, err := s.AgentGet("agent-los32d-resume")
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	want := filepath.Join(RuntimeDir(h.dataDir), runWorkDirName, jobDirName("agent-los32d-resume"))
	if got.WorkDir != want {
		t.Fatalf("work_dir = %q, want the run dir %q", got.WorkDir, want)
	}
}

// The agent resolver must not write the allow-all permissions into the user's
// home: a settings.local.json there applies to every claude session.
func TestLos32dAgentWorkDirWritesNoPermissionsIntoTheHome(t *testing.T) {
	home := isolateHome(t)
	h, _ := los32dHandler(t)

	h.resolveAgentWorkDir(home, home)

	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Fatalf("an allow-all settings.local.json was written into %s", home)
	}
}

// A home reached through a symlink is still the home: the guard compares resolved
// paths on both sides.
func TestLos32dJobProjectIsRefusedWhenTheHomeIsASymlink(t *testing.T) {
	real := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink home: %v", err)
	}
	isolateHomeAt(t, link)

	if jobProjectUsable(real) {
		t.Fatalf("jobProjectUsable(%q) = true while $HOME is the symlink %q to it", real, link)
	}
}

// Deleting a job must not remove the directory a live run may be sitting in: the
// run work dir is the cwd of the job's process, and a job started by hand never
// enters the scheduler's running set.
func TestLos32dRemoveJobDirKeepsTheRunWorkDir(t *testing.T) {
	h, _ := los32dHandler(t)
	runDir, err := EnsureRunWorkDir(h.dataDir, "sched-los32d-delete")
	if err != nil {
		t.Fatalf("EnsureRunWorkDir: %v", err)
	}
	logDir := filepath.Join(RuntimeDir(h.dataDir), "jobs", jobDirName("sched-los32d-delete"))
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}

	h.removeJobDir("sched-los32d-delete")

	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("the run work dir was removed with the job: %v", err)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Errorf("the log dir survived job deletion: %v", err)
	}
}

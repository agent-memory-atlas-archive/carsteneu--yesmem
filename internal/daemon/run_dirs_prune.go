package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runDirMinAge is how long an unreferenced run directory is left alone before the
// sweep removes it. The registry of in-flight runs (activeRuns) is the primary
// protection for a live cwd; the age only has to cover what nobody registered: a
// run started by a daemon that has since crashed, or a hand-started job whose row
// was deleted. The directory's own mtime is a weak signal (it moves only when a
// top-level entry changes), so it is a grace, not a proof of death.
var runDirMinAge = time.Hour

// runDirSweepPrefixes are the key families whose directories this sweep owns. A
// job ID starts with "sched-" (scheduleCreate) and an agent ID with "agent-", so
// every directory below the run-work-dir base belongs to one of them or is left
// alone. Cap runs key their directory on "cap:<name>" (capExecWorkDir) and have no
// row in any table: that directory is their workspace between runs, so the sweep
// must not treat it as an orphan.
var runDirSweepPrefixes = []string{"sched-", "agent-"}

// pruneRunDirs removes the per-key directories the daemon created for runs whose
// key is gone: the job log dirs and the run work dirs. Nothing else ever cleaned
// them up, so a project-less job or a crashed agent left an empty 0700 directory
// behind for good (YM-295).
//
// The live set comes from the database, not from the scheduler: a daemon restart
// reloads the job rows, and an agent row outlives its process (it is resumable).
// Without a complete live set nothing is removed; a sweep on a partial set is a
// guess, and the guess would delete a running process's cwd.
func (h *Handler) pruneRunDirs() {
	if h.dataDir == "" {
		return
	}
	live := make(map[string]bool)
	jobs, err := h.store.ListScheduledJobs()
	if err != nil {
		log.Printf("[heartbeat] run dir sweep skipped: scheduled jobs: %v", err)
		return
	}
	for _, job := range jobs {
		live[jobDirName(jobKey(job.ID, job.Name))] = true
	}
	agents, err := h.store.AgentList("")
	if err != nil {
		log.Printf("[heartbeat] run dir sweep skipped: agents: %v", err)
		return
	}
	workBase := runWorkDirBase(h.dataDir)
	for _, agent := range agents {
		live[jobDirName(agent.ID)] = true
		// A scheduler agent's cwd is keyed on the JOB that spawned it, so its row
		// points at the directory instead of naming its key: protect what the row
		// says, not only the name the row would produce.
		if dir := filepath.Clean(agent.WorkDir); agent.WorkDir != "" && filepath.Dir(dir) == workBase {
			live[filepath.Base(dir)] = true
		}
	}
	// A run in flight has no row that vouches for its directory: a job deleted
	// during its run is gone from scheduled_jobs while the process keeps writing.
	h.activeRunsMu.Lock()
	for key := range h.activeRuns {
		live[jobDirName(key)] = true
	}
	h.activeRunsMu.Unlock()

	bases := []string{
		filepath.Join(RuntimeDir(h.dataDir), "jobs"),
		workBase,
	}
	removed := 0
	for _, base := range bases {
		// A base that is not a real directory is skipped, never followed: the
		// runtime dir is same-uid server state and a run's cwd sits one level below
		// it, so a symlink here would have ReadDir list, and RemoveAll delete,
		// paths outside the runtime dir. Same rule as EnsureRuntimeDir and
		// EnsureRunWorkDir.
		if info, err := os.Lstat(base); err != nil || !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || live[entry.Name()] || !ownedRunDirName(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) < runDirMinAge {
				continue
			}
			dir := filepath.Join(base, entry.Name())
			// %q: the name is bytes from the filesystem and may carry a line break,
			// which would forge a log line (the sibling removeJobDir log only carries
			// the sanitized jobDirName).
			if err := os.RemoveAll(dir); err != nil {
				log.Printf("[heartbeat] run dir sweep: remove %q: %v", dir, err)
				continue
			}
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[heartbeat] removed %d run directories that belonged to no job and no agent", removed)
	}
}

// ownedRunDirName reports whether a directory name was produced by jobDirName for
// a key this sweep owns.
func ownedRunDirName(name string) bool {
	for _, prefix := range runDirSweepPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// runStarting records that a run with this directory key is in flight. Paired with
// runFinished; a counter, so two runs of the same key cannot clear each other.
func (h *Handler) runStarting(key string) {
	if key == "" {
		return
	}
	h.activeRunsMu.Lock()
	if h.activeRuns == nil {
		h.activeRuns = make(map[string]int)
	}
	h.activeRuns[key]++
	h.activeRunsMu.Unlock()
}

func (h *Handler) runFinished(key string) {
	if key == "" {
		return
	}
	h.activeRunsMu.Lock()
	if n := h.activeRuns[key]; n > 1 {
		h.activeRuns[key] = n - 1
	} else {
		delete(h.activeRuns, key)
	}
	h.activeRunsMu.Unlock()
}

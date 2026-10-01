package codescan

import (
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cbmIndexBudget bounds one background reindex per project (user decision:
// yesloop-cbm-load-fix D3). A full CBM index can take minutes; the budget
// keeps runaway index runs from occupying the machine indefinitely.
const cbmIndexBudget = 15 * time.Minute

var (
	indexFlightMu sync.Mutex
	indexFlight   = map[string]bool{}
	indexWorker   = func(rootDir string) error { return runIndexProject(rootDir) }

	// attemptedRefresh remembers per project the HEAD for which a stale
	// refresh was already triggered — failed/no-op index runs must not
	// re-trigger on every 5-min tick for the same HEAD.
	attemptedRefresh = map[string]string{}

	// ensureTrigger injectable for tests.
	ensureTrigger = func(rootDir string) { EnsureIndex(rootDir) }
)

// GitHeadTime returns the commit time of HEAD, zero on any error.
func GitHeadTime(rootDir string) time.Time {
	out, err := exec.Command("git", "-C", rootDir, "log", "-1", "--format=%ct").Output()
	if err != nil {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// EnsureIndexIfStale triggers a background reindex when the CBM index is
// older than the HEAD commit time — code tools must not serve a stale graph
// after a commit (user decision: no time-based rescan cap, freshness coupled
// to git HEAD + CBM mtime). At most one attempt per (project, HEAD): CBM may
// not touch the DB mtime when nothing changed, and a failed index must not
// re-trigger on every tick. Failures go through the existing scan cooldown.
func EnsureIndexIfStale(rootDir, head string) {
	if head == "" {
		return
	}
	mtime := CBMIndexMtime(rootDir)
	if !mtime.IsZero() && !mtime.Before(GitHeadTime(rootDir)) {
		return // fresh — index newer than or equal to HEAD commit
	}
	project := cbmProjectName(rootDir)

	indexFlightMu.Lock()
	if attemptedRefresh[project] == head {
		indexFlightMu.Unlock()
		return
	}
	attemptedRefresh[project] = head
	indexFlightMu.Unlock()

	ensureTrigger(rootDir)
}

// EnsureIndex starts a background reindex for rootDir, single-flight per
// project: an already running attempt wins, the trigger is dropped. Safe for
// the scan path — it never blocks and never spawns a second index run.
func EnsureIndex(rootDir string) {
	project := cbmProjectName(rootDir)

	indexFlightMu.Lock()
	if indexFlight[project] {
		indexFlightMu.Unlock()
		return
	}
	indexFlight[project] = true
	indexFlightMu.Unlock()

	go func() {
		defer func() {
			indexFlightMu.Lock()
			delete(indexFlight, project)
			indexFlightMu.Unlock()
		}()
		start := time.Now()
		if err := indexWorker(rootDir); err != nil {
			log.Printf("cbm: background index %s failed after %s: %v", project, time.Since(start).Round(time.Second), err)
			return
		}
		log.Printf("cbm: background index %s done in %s", project, time.Since(start).Round(time.Second))
	}()
}

// runIndexProject indexes rootDir synchronously via the CBM CLI.
func runIndexProject(rootDir string) error {
	bin := FindCBMBinary()
	if bin == "" {
		return fmt.Errorf("cbm: codebase-memory-mcp binary not found")
	}
	return cbmIndexRepository(bin, rootDir, cbmIndexBudget)
}

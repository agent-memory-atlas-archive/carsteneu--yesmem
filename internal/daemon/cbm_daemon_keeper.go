package daemon

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/storage"
)

// cbmDaemonCheckInterval: how often the keeper re-runs the idempotent CBM
// `daemon start` after a healthy state. A crashed permanent daemon restarts
// on its own; without it every CLI call falls back to one internal daemon
// spawn per query.
const cbmDaemonCheckInterval = 5 * time.Minute

// cbmDaemonRetryBackoff: fast retry while a session-managed daemon holds the
// coordination slot — the keeper wants the permanent daemon (the main load
// relief) up as soon as the session daemon retires.
const cbmDaemonRetryBackoff = 30 * time.Second

func startCBMDaemonKeeper(ctx context.Context, store *storage.Store) {
	bin := codescan.FindCBMBinary()
	if bin == "" {
		log.Printf("cbm: daemon keeper: codebase-memory-mcp not installed")
		return
	}
	restoreGitSwaps(ctx, store)
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			err := codescan.EnsureCBMDaemon(bin)
			switch {
			case err == nil:
				timer.Reset(cbmDaemonCheckInterval)
			case errors.Is(err, codescan.ErrDaemonSessionManaged):
				log.Printf("cbm: daemon keeper: session-managed, retry in %s", cbmDaemonRetryBackoff)
				timer.Reset(cbmDaemonRetryBackoff)
			default:
				log.Printf("cbm: daemon keeper: failed: %v", err)
				timer.Reset(cbmDaemonCheckInterval)
			}
		}
	}()
}

// restoreGitSwaps repairs legacy .git swaps left by crashed index calls
// (older yesmem builds renamed .git to .git.yesmem-bak inside the worktree).
// Checked once at daemon boot over all active projects.
func restoreGitSwaps(ctx context.Context, store *storage.Store) {
	projects, err := activeProjects(ctx, store)
	if err != nil {
		log.Printf("cbm: git swap recovery: list projects: %v", err)
		return
	}
	for _, dir := range projects {
		if codescan.RestoreLegacyGitSwap(dir) {
			log.Printf("cbm: git swap recovery: repaired leftover swap for %s", dir)
		}
	}
}

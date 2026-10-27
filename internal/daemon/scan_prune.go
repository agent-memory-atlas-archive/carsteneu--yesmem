package daemon

import (
	"context"
	"log"
	"time"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/storage"
)

const (
	// scanPruneInterval bounds how often the sweep runs.
	scanPruneInterval = 24 * time.Hour
	// scanPruneMaxAge is far above the 10 minute scan cooldown: a row this old
	// cannot suppress a retry any more, it is only dead weight.
	scanPruneMaxAge = 24 * time.Hour
)

// startScanPruneTicker drops scan bookkeeping for projects that no longer
// exist: removed worktrees leave project_scan rows behind, and old-scheme
// scan_cooldown rows keyed by bare project name never expire on their own.
func startScanPruneTicker(ctx context.Context, store *storage.Store) {
	go func() {
		pruneScanState(store)
		ticker := time.NewTicker(scanPruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pruneScanState(store)
			}
		}
	}()
}

func pruneScanState(store *storage.Store) {
	scans, cooldowns, err := store.PruneOrphanedScanState(scanPruneMaxAge, codescan.ScanCacheKey)
	if err != nil {
		log.Printf("scan-prune: %v", err)
		return
	}
	if scans > 0 || cooldowns > 0 {
		log.Printf("scan-prune: removed %d project_scan rows, %d scan_cooldown rows", scans, cooldowns)
	}
}

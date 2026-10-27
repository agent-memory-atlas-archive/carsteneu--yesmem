package daemon

import (
	"context"
	"log"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// sessionPinSweepInterval is how often expired session pins are removed. The
// newest row is at most SessionPinMaxAge old when it would qualify, so an hourly
// tick bounds the visible lifetime to the TTL plus one interval.
const sessionPinSweepInterval = time.Hour

// startSessionPinSweep removes session pins that outlived maxAge. Only the Claude
// Code SessionEnd hook clears session pins on /clear; opencode and codex sessions
// have no such event, so their pins would otherwise stay in every briefing of the
// project forever. Attributed rows expire by age, and so do project-less rows
// without attribution: they are global and no event can reach them. Project-bound
// rows without attribution keep their own path, which /clear removes.
func startSessionPinSweep(ctx context.Context, store *storage.Store, maxAge time.Duration) {
	sweep := func() {
		cutoff := time.Now().Add(-maxAge)
		removed, err := store.PurgeSessionPinsBefore(cutoff)
		if err != nil {
			log.Printf("[pins] purge expired session pins: %v", err)
			return
		}
		if removed > 0 {
			log.Printf("[pins] purged %d session pins created before %s",
				removed, cutoff.UTC().Format(time.RFC3339))
		}
	}

	sweep()
	ticker := time.NewTicker(sessionPinSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

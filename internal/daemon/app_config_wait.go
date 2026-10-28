package daemon

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/update"
)

// Constants for the update ticker's wait for the config load.
const (
	updateCfgPollInterval = 5 * time.Second
	updateCfgWaitTimeout  = 30 * time.Minute
)

// updateTickerDecision waits for the config and reports whether the auto-update
// ticker should run, and at which interval. Both negative outcomes are logged:
// a daemon whose auto-update never starts must say why instead of exiting
// silently, which is what the old immediate appCfg read did.
func updateTickerDecision(ctx context.Context, mu *sync.Mutex, appCfg **config.Config) (time.Duration, bool) {
	log.Println("[update] auto-update ticker starting")
	ac := waitForAppConfig(ctx, mu, appCfg, updateCfgPollInterval, updateCfgWaitTimeout)
	if ac == nil {
		// A shutdown is not a config problem.
		if ctx.Err() == nil {
			log.Println("[update] auto-update check not started: config was not loaded")
		}
		return 0, false
	}
	if !ac.Update.AutoUpdate {
		log.Println("[update] auto-update disabled in config")
		return 0, false
	}
	interval := update.ParseCheckInterval(ac.Update.CheckInterval)
	log.Printf("[update] auto-update enabled, checking every %v", interval)
	return interval, true
}

// waitForAppConfig blocks until the async startup goroutine has published the
// loaded config. Every ticker that needs config values starts before that
// publication (the load runs after the initial session indexing), so reading the
// pointer once at goroutine start always sees nil — the update ticker exited
// silently that way.
//
// The wait is bounded between polls: an indexing run that never finishes must
// not hold the caller forever, and a nil return is the signal to log and give up.
// The deadline is checked before each lock acquisition, so the bound holds as long
// as the guarded sections stay short — they only copy the config pointer.
func waitForAppConfig(ctx context.Context, mu *sync.Mutex, appCfg **config.Config, poll, timeout time.Duration) *config.Config {
	deadline := time.Now().Add(timeout)
	for {
		mu.Lock()
		ac := *appCfg
		mu.Unlock()
		if ac != nil {
			return ac
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poll):
		}
	}
}

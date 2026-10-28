package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
)

// The daemon loads its config inside the async startup goroutine, after the
// initial session indexing. The update ticker starts before that publication, so
// reading appCfg once at goroutine start always saw nil and the ticker returned
// without a single log line (audit: zero [update] lines). The waiter is what
// makes the ticker survive that ordering.
func TestWaitForAppConfig(t *testing.T) {
	t.Run("published after startup", func(t *testing.T) {
		var (
			mu  sync.Mutex
			cfg *config.Config
		)
		go func() {
			time.Sleep(20 * time.Millisecond)
			loaded := config.Default()
			loaded.Update.AutoUpdate = true
			mu.Lock()
			cfg = loaded
			mu.Unlock()
		}()

		got := waitForAppConfig(context.Background(), &mu, &cfg, 5*time.Millisecond, 2*time.Second)
		if got == nil {
			t.Fatal("waiter gave up before the config was published")
		}
		if !got.Update.AutoUpdate {
			t.Error("waiter returned a config other than the published one")
		}
	})

	t.Run("already published", func(t *testing.T) {
		var (
			mu  sync.Mutex
			cfg = config.Default()
		)
		if got := waitForAppConfig(context.Background(), &mu, &cfg, time.Millisecond, time.Second); got != cfg {
			t.Error("waiter must return the config that is already published")
		}
	})

	t.Run("timeout without publication", func(t *testing.T) {
		var (
			mu  sync.Mutex
			cfg *config.Config
		)
		start := time.Now()
		if got := waitForAppConfig(context.Background(), &mu, &cfg, 2*time.Millisecond, 30*time.Millisecond); got != nil {
			t.Error("waiter returned a config that was never published")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("waiter ignored its timeout, waited %v", elapsed)
		}
	})

	t.Run("context cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var (
			mu  sync.Mutex
			cfg *config.Config
		)
		if got := waitForAppConfig(ctx, &mu, &cfg, time.Hour, time.Hour); got != nil {
			t.Error("waiter must return nil when the daemon is shutting down")
		}
	})
}

// The decision is what the daemon's ticker goroutine acts on: without it the
// goroutine returned silently on a nil config and no [update] line ever reached
// the log.
func TestUpdateTickerDecision(t *testing.T) {
	published := func(autoUpdate bool, interval string) (*sync.Mutex, *(*config.Config)) {
		ac := config.Default()
		ac.Update.AutoUpdate = autoUpdate
		ac.Update.CheckInterval = interval
		var (
			mu  sync.Mutex
			cfg *config.Config = ac
		)
		return &mu, &cfg
	}

	t.Run("auto-update disabled", func(t *testing.T) {
		mu, cfg := published(false, "3h")
		interval, enabled := updateTickerDecision(context.Background(), mu, cfg)
		if enabled || interval != 0 {
			t.Errorf("got (interval=%v, enabled=%v), want the ticker not to start", interval, enabled)
		}
	})

	t.Run("auto-update enabled with configured interval", func(t *testing.T) {
		mu, cfg := published(true, "3h")
		interval, enabled := updateTickerDecision(context.Background(), mu, cfg)
		if !enabled {
			t.Fatal("ticker must start when auto_update is on")
		}
		if interval != 3*time.Hour {
			t.Errorf("interval = %v, want 3h", interval)
		}
	})

	t.Run("auto-update enabled without interval uses the default", func(t *testing.T) {
		mu, cfg := published(true, "")
		interval, enabled := updateTickerDecision(context.Background(), mu, cfg)
		if !enabled {
			t.Fatal("ticker must start when auto_update is on")
		}
		if interval != 6*time.Hour {
			t.Errorf("interval = %v, want the 6h default", interval)
		}
	})

	t.Run("config never published", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var (
			mu  sync.Mutex
			cfg *config.Config
		)
		interval, enabled := updateTickerDecision(ctx, &mu, &cfg)
		if enabled || interval != 0 {
			t.Errorf("got (interval=%v, enabled=%v), want no ticker without a config", interval, enabled)
		}
	})
}

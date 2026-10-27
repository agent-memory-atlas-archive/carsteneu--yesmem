package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-280 (COG-C24): only the Claude Code SessionEnd hook calls ClearSessionPins,
// so opencode and codex session pins are never cleared. The daemon sweeps them by
// age instead. A negative maxAge puts the cutoff in the future, so a pin written
// just now counts as expired — the immediate sweep is what this test proves.
func TestStartSessionPinSweepPurgesExpiredPins(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.PinLearning("session", "", "opencode session pin", "test", "ses_abc"); err != nil {
		t.Fatalf("seed pin: %v", err)
	}
	if pins, err := s.GetPinnedLearnings("session", ""); err != nil || len(pins) != 1 {
		t.Fatalf("seeded pin not readable: %d pins, err %v", len(pins), err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	startSessionPinSweep(ctx, s, -time.Minute)

	pins, err := s.GetPinnedLearnings("session", "")
	if err != nil {
		t.Fatalf("get pins: %v", err)
	}
	if len(pins) != 0 {
		t.Errorf("expired session pin survived the sweep: %v", pins)
	}
}

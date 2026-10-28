package setup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// YM-333 (Los 38o): `systemctl --user daemon-reload` ran without a context. The
// call talks to the user manager over D-Bus; when that manager is wedged it does
// not return, and the post-update migration sits there with no output and no
// timeout. exec.CommandContext with a deadline is the difference between a slow
// migration and a hung one.
func TestReloadSystemdUnitsBoundsTheCall(t *testing.T) {
	var (
		gotCtx  context.Context
		gotArgs []string
	)
	prev := systemctlRun
	systemctlRun = func(ctx context.Context, args ...string) error {
		gotCtx, gotArgs = ctx, args
		return nil
	}
	t.Cleanup(func() { systemctlRun = prev })

	if err := ReloadSystemdUnits(); err != nil {
		t.Fatalf("ReloadSystemdUnits: %v", err)
	}
	if gotCtx == nil {
		t.Fatal("no context reached systemctl: the call is unbounded and can hang on a wedged user manager")
	}
	deadline, ok := gotCtx.Deadline()
	if !ok {
		t.Fatal("context carries no deadline, so a wedged systemctl blocks the migration forever")
	}
	if left := time.Until(deadline); left <= 0 || left > systemdReloadTimeout {
		t.Errorf("deadline lies %v ahead, want at most %v", left, systemdReloadTimeout)
	}
	if got := strings.Join(gotArgs, " "); got != "--user daemon-reload" {
		t.Errorf("systemctl args = %q, want %q", got, "--user daemon-reload")
	}
}

// TestReloadSystemdUnitsReportsAFailure: the reload is best effort at the call
// site, but it must not report success when systemd refused.
func TestReloadSystemdUnitsReportsAFailure(t *testing.T) {
	prev := systemctlRun
	systemctlRun = func(context.Context, ...string) error {
		return errors.New("Failed to connect to bus: No such file or directory")
	}
	t.Cleanup(func() { systemctlRun = prev })

	if err := ReloadSystemdUnits(); err == nil {
		t.Fatal("ReloadSystemdUnits reported success although systemctl failed")
	}
}

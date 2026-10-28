package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/carsteneu/yesmem/internal/opencode"
)

// TestMain keeps the package's unit tests hermetic: the on-demand opencode
// bind is reached from get_agent, send_to, whoami and _track_usage, and its
// default lookup would query the user's live opencode service. Tests that
// exercise the bind stub it explicitly (stubOpencodeFind).
//
// The delivery seam needs the same treatment for the same reason: send_to
// pushes to an opencode session through whatever service opencodeV2Service
// discovers, so a test addressing an opencode session would otherwise file a
// prompt in the developer's live session. Tests that want a service install a
// stub (stubV2Service); the rest see "no service running", which is also the
// v1-only install.
func TestMain(m *testing.M) {
	prepareOpencodeAgentServer = func(string) ([]string, []string, error) { return nil, nil, nil }
	opencodeFindSessionFn = func(context.Context, opencode.MarkerQuery) (string, error) {
		return "", nil
	}
	opencodeV2Service = func() *opencode.Client { return nil }
	// The heartbeat guards ask whether a task() subagent is running; the default
	// lookup queries the developer's live opencode.db. The idle guard reads it on
	// every sweep (YM-253), so the pin is suite-wide now, not only where a test
	// exercises the freeze — those stub it explicitly with
	// stubOpencodeChildActive. Tests of the gate itself restore the real
	// lookup with restoreRealOpencodeChildActive.
	realOpencodeChildActiveFn = opencodeChildActiveFn
	opencodeChildActiveFn = func(string) bool { return false }
	// The daemon-restart recovery asks whether an agent's opencode session still
	// exists; the default lookup reads the developer's live opencode.db. See
	// opencode_resumable_test.go for the gate's own test.
	opencodeSessionResumableFn = func(string, string) (bool, error) { return false, nil }
	// The restart gate resolves a vanished work dir through the directory of the
	// backend session, which reads the same live database. Tests that want the
	// branch install their own answer (see los38s_agent_rows_test.go).
	opencodeSessionDirFn = func(string) string { return "" }
	os.Exit(m.Run())
}

// realOpencodeChildActiveFn is the production child lookup as TestMain found it.
var realOpencodeChildActiveFn func(string) bool

// restoreRealOpencodeChildActive re-installs the production lookup for a test
// that exercises the gate itself rather than the guards that consume it.
func restoreRealOpencodeChildActive(t *testing.T) {
	t.Helper()
	pinned := opencodeChildActiveFn
	opencodeChildActiveFn = realOpencodeChildActiveFn
	t.Cleanup(func() { opencodeChildActiveFn = pinned })
}

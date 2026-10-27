package daemon

import "testing"

// requireProcTable skips a test that supervises a process. Liveness, identity,
// start time and peer credentials all come from /proc, and darwin exposes no
// process table, so the supervision paths cannot answer there. The guard keeps
// the reason in the output instead of a failure that reads like a bug.
func requireProcTable(t *testing.T) {
	t.Helper()
	if !procTableReadable {
		t.Skip("the process table (/proc) is not available on this platform")
	}
}

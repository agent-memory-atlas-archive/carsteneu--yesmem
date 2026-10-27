package proxy

import (
	"os"
	"testing"
)

// shortSocketDir returns a temp dir short enough to hold a unix socket on
// darwin. macOS caps sun_path at 104 bytes, and t.TempDir() nests the test name
// under /var/folders/<...>/T, which overruns that limit: bind and connect then
// fail with "invalid argument" before the test logic ever runs.
func shortSocketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "ym")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

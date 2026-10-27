package codescan

import (
	"path/filepath"
	"testing"
)

// realPath returns p with symlinks resolved, the form the scanners report: they
// canonicalise the root they hand back, and so do the cache keys. macOS reaches
// its temp root through /var -> /private/var, so a test that compares against the
// raw t.TempDir() string fails there alone — the fixture has to be built on the
// resolved path.
func realPath(t *testing.T, p string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("resolve %s: %v", p, err)
	}
	return resolved
}

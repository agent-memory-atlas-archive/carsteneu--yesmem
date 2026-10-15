package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// stubHelpExec replaces the `--help` runner for the duration of a test and
// clears the capability cache on both ends so tests never observe each other.
// It also installs a stub identity, so a test can exercise the help parsing
// without a real binary on disk; call stubBinaryIdentity afterwards to drive
// the cache-key path instead.
func stubHelpExec(t *testing.T, fn func(bin string) (string, error)) {
	t.Helper()
	originalHelp := runHelpOutput
	originalIdentity := binaryIdentity
	runHelpOutput = fn
	binaryIdentity = func(string) (string, bool) { return "stub-identity", true }
	resetThemeFlagCache()
	t.Cleanup(func() {
		runHelpOutput = originalHelp
		binaryIdentity = originalIdentity
		resetThemeFlagCache()
	})
}

// stubUserHome points userHomeDir at a temporary home and installs the given
// yesmem theme names under ~/.config/opencode/themes (empty = nothing
// installed).
func stubUserHome(t *testing.T, themes ...string) {
	t.Helper()
	home := t.TempDir()
	for _, name := range themes {
		dst := filepath.Join(home, ".config", "opencode", "themes", name+".json")
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	original := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = original })
}

// stubBinaryIdentity replaces the cache-key builder (mtime+size) for the
// duration of a test, so the binary-replacement path is testable without
// touching the filesystem.
func stubBinaryIdentity(t *testing.T, fn func(path string) (string, bool)) {
	t.Helper()
	original := binaryIdentity
	binaryIdentity = fn
	resetThemeFlagCache()
	t.Cleanup(func() {
		binaryIdentity = original
		resetThemeFlagCache()
	})
}

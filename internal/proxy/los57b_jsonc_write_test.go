package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-376 (PRX-P47, 1+2): the atomic config write must hand back a file with the
// configured mode, the new content, and no temp file left behind. The mode is
// applied after the write (before it, the temp file sat at the target mode while
// it was still being filled), and the parent directory is synced so the rename
// survives a crash.
func TestLos57bWriteFileAtomicKeepsModeContentAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := writeFileAtomic(path, []byte(`{"new":true}`), 0o640); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != `{"new":true}` {
		t.Errorf("content = %q, want the new payload", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("mode = %o, want the configured 640", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "opencode.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only opencode.json (no temp leftover)", names)
	}
}

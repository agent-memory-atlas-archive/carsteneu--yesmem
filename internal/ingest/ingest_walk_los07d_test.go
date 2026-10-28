package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-324 [DEV-DT50]: the walk callback returned the ReadDir error of a single
// subdirectory, which aborted the whole run. In the daemon's doc-sync pass that
// also cost every source that came after it in the same loop. The readable part
// of the tree must still be indexed.
func TestDocIngest_UnreadableSubdirectoryDoesNotAbortTheRunLos07d(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	store := mustOpenStore(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readable.md"), []byte("# Readable\n\nProse that must be indexed."), 0644); err != nil {
		t.Fatalf("write readable: %v", err)
	}
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blocked, 0755); err != nil {
		t.Fatalf("mkdir blocked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "hidden.md"), []byte("# Hidden\n\nUnreadable prose."), 0644); err != nil {
		t.Fatalf("write hidden: %v", err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatalf("chmod blocked: %v", err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0755) })

	result, err := Run(Config{Project: "test-proj", Name: "walk-docs"}, []string{dir}, store)
	if err != nil {
		t.Fatalf("one unreadable subdirectory aborted the whole ingest: %v", err)
	}
	if result.FilesProcessed != 1 {
		t.Errorf("processed = %d, want 1 (the readable document)", result.FilesProcessed)
	}
	// The dropped subtree has to be visible: a caller that only sees
	// "1 processed, 0 skipped" reads a complete run.
	if result.UnreadableEntries != 1 {
		t.Errorf("unreadable entries = %d, want 1 (the blocked subdirectory)", result.UnreadableEntries)
	}
}

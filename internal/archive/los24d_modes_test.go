package archive

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-338 (Los 24d): the archived transcript copy is a second copy of the
// conversation, and it was created with the process umask (0644). The 0700 data
// directory hid that, but a copy that only looks private because of its parent
// is one move away from being readable.
func TestArchivedTranscriptCopyIsOwnerOnly(t *testing.T) {
	srcDir := t.TempDir()
	archiveDir := t.TempDir()
	src := filepath.Join(srcDir, "session.jsonl")
	if err := os.WriteFile(src, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	a := New(archiveDir)
	if err := a.ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	archived := filepath.Join(archiveDir, "proj", "session.jsonl")
	assertOwnerOnly(t, archived)
}

// The append path opens an existing copy. A copy written by an older version is
// 0644 already, so appending has to restrict it as well.
func TestArchivedTranscriptAppendRestrictsAnExistingCopy(t *testing.T) {
	srcDir := t.TempDir()
	archiveDir := t.TempDir()
	src := filepath.Join(srcDir, "session.jsonl")
	if err := os.WriteFile(src, []byte("first"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	a := New(archiveDir)
	if err := a.ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("first archive: %v", err)
	}

	archived := filepath.Join(archiveDir, "proj", "session.jsonl")
	// An older installation left this behind as 0644.
	if err := os.Chmod(archived, 0o644); err != nil {
		t.Fatalf("chmod copy: %v", err)
	}

	if err := os.WriteFile(src, []byte("first and more"), 0o644); err != nil {
		t.Fatalf("grow source: %v", err)
	}
	if err := a.ArchiveFile(src, "proj"); err != nil {
		t.Fatalf("append to archive: %v", err)
	}

	assertOwnerOnly(t, archived)
}

func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("%s is %04o, want no group or other permission bits", path, mode)
	}
}

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-231: the daemon's doc-sync re-read a relative source path against its own
// working directory, which has nothing to do with the directory the source was
// registered from. A tree that happens to share the name fed foreign content
// into the source.
func TestRunDocSync_RefusesRelativeSourcePath(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	decoy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(decoy, "docs"), 0755); err != nil {
		t.Fatalf("mkdir decoy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "docs", "decoy.md"), []byte("# Decoy\n\nForeign content that must not be indexed."), 0644); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	t.Chdir(decoy)

	if _, err := s.UpsertDocSource(&storage.DocSource{Name: "relative-docs", Project: "p", Path: "docs"}); err != nil {
		t.Fatalf("upsert source: %v", err)
	}

	runDocSync(s, nil)

	src, err := s.GetDocSource("relative-docs", "p")
	if err != nil {
		t.Fatalf("source vanished: %v", err)
	}
	chunks, err := s.GetDocChunksBySource(src.ID)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("%d chunk(s) created from the cwd-relative path %q in the daemon sync", len(chunks), src.Path)
	}
}

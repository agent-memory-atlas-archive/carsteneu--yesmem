package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/ingest"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los07fCaptureLog redirects the standard logger into a buffer and restores the
// previous writer and flags when the test ends.
func los07fCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevWriter, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// los07fSealDir makes a directory unreadable for its owner, so a walk of it
// fails with a real I/O error instead of "not found".
func los07fSealDir(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("seal %s: %v", path, err)
	}
	t.Cleanup(func() {
		// The walk failed, so nothing indexed the tree; the mode only has to
		// come back for t.TempDir's removal.
		os.Chmod(path, 0755)
	})
}

// YM-364 [DEV-DT57]: runDocSync logged src.Path and the ingest error with %s
// and %v. Both are caller-chosen, so a newline in either forged a log line. The
// same injection type was closed in ingest.go for the ingest side only.
func TestRunDocSync_LogQuotesCallerChosenPathAndErrorLos07f(t *testing.T) {
	s := mustStore(t)
	buf := los07fCaptureLog(t)

	base := t.TempDir()

	missing := filepath.Join(base, "gone\nFORGED-MISSING")
	if _, err := s.UpsertDocSource(&storage.DocSource{Name: "log-missing", Project: "p", Path: missing}); err != nil {
		t.Fatalf("upsert missing source: %v", err)
	}

	// A sealed directory is the only way to make the walk itself fail, and
	// chmod does not deny root: the missing-path half keeps running there, and
	// the log line names the dropped half so a root run does not read as full
	// coverage.
	walkErr := false
	if os.Geteuid() != 0 {
		sealed := filepath.Join(base, "sealed\nFORGED-WALK")
		if err := os.MkdirAll(sealed, 0755); err != nil {
			t.Fatalf("mkdir sealed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sealed, "doc.md"), []byte("# Doc\n\nProse that the sealed walk never reaches."), 0644); err != nil {
			t.Fatalf("write sealed doc: %v", err)
		}
		los07fSealDir(t, sealed)
		if _, err := s.UpsertDocSource(&storage.DocSource{Name: "log-walk", Project: "p", Path: sealed}); err != nil {
			t.Fatalf("upsert sealed source: %v", err)
		}
		walkErr = true
	} else {
		t.Log("running as root: the sealed-walk half of this test is skipped, only the missing-path quoting is proven")
	}

	runDocSync(s, nil)

	out := buf.String()
	if strings.Contains(out, "\nFORGED") {
		t.Errorf("a raw newline from a caller-chosen path forged a log line:\n%s", out)
	}
	if !strings.Contains(out, "skipped missing path") {
		t.Fatalf("the missing path was not logged at all:\n%s", out)
	}
	if walkErr && !strings.Contains(out, "error:") {
		t.Fatalf("the failed walk was not logged at all:\n%s", out)
	}
}

// YM-364 [DEV-DT57]: a source whose path is empty never reached ingest.Run, so
// nothing compared its documents against a tree and its chunks stayed in the
// index forever. The retrieval side kept serving text that no ingest can ever
// refresh.
func TestRunDocSync_SweepsChunksOfASourceWithoutPathLos07f(t *testing.T) {
	s := mustStore(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n\nProse of a source that loses its path."), 0644); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	if _, err := ingest.Run(ingest.Config{Project: "p", Name: "lost-path"}, []string{dir}, s); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}
	src, err := s.GetDocSource("lost-path", "p")
	if err != nil || src == nil {
		t.Fatalf("get doc source: %v (source=%v)", err, src)
	}
	if chunks, _ := s.GetDocChunksBySource(src.ID); len(chunks) == 0 {
		t.Fatal("seed ingest did not index the source")
	}

	// The registration loses its path: no tree is left to compare against.
	if _, err := s.UpsertDocSource(&storage.DocSource{Name: "lost-path", Project: "p", Path: ""}); err != nil {
		t.Fatalf("clear source path: %v", err)
	}

	// The sync tick only runs when the gate reports work. A path-less source is
	// not a change signal, but its chunks are still there and only that tick
	// sweeps them.
	if !hasDocSyncWork(s) {
		t.Fatal("a source without a path still holds documents, but the gate reports no work for it")
	}

	los07fCaptureLog(t)
	runDocSync(s, nil)

	if chunks, err := s.GetDocChunksBySource(src.ID); err != nil {
		t.Fatalf("chunks: %v", err)
	} else if len(chunks) != 0 {
		t.Errorf("%d chunk(s) of a source without a path survived the sync", len(chunks))
	}
	if hasDocSyncWork(s) {
		t.Error("the gate still reports work for a source without a path after its chunks were swept")
	}
}

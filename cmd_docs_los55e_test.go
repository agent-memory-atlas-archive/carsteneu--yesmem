package main

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

// YM-374 [DEV-DT59]: the CLI sync skipped a source without a path with a log
// line and nothing else. The daemon's doc-sync sweeps its chunks, because a
// source without a path has no tree that could refresh them; in the CLI that
// invariant only held once the daemon ticked. The chunks of such a source are
// reachable through docs_search until then, although nothing can update them.
func TestDocsCLI_SyncSweepsSourceWithoutPathLos55e(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedDocsDB(t, home, storage.DocSource{Name: "orphan-docs", Project: "p"})

	store := openDocsDB(t, home)
	src, err := store.GetDocSource("orphan-docs", "p")
	if err != nil || src == nil {
		t.Fatalf("get source: %v (source=%v)", err, src)
	}
	if src.Path != "" {
		t.Fatalf("registered path = %q, want empty for this case", src.Path)
	}
	for _, file := range []string{"guide.md", "reference.md"} {
		if _, err := store.InsertDocChunk(&storage.DocChunk{
			SourceID:    src.ID,
			SourceFile:  file,
			SourceHash:  "hash-" + file,
			Content:     "Prose of " + file,
			ContentHash: "content-" + file,
		}); err != nil {
			t.Fatalf("insert chunk %s: %v", file, err)
		}
	}

	var logBuf bytes.Buffer
	prevLog := log.Default().Writer()
	log.SetOutput(&logBuf)
	captureOutput(t, func() { runSyncDocs([]string{"--all"}) })
	log.SetOutput(prevLog)

	chunks, err := store.GetDocChunksBySource(src.ID)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("%d chunk(s) of a source without a path survived the sync; nothing can ever refresh them", len(chunks))
	}
	if !strings.Contains(logBuf.String(), "orphan-docs") {
		t.Errorf("the sweep did not name the source:\n%s", logBuf.String())
	}
}

// The re-read guard: the caller's source list is a snapshot taken before the
// loop spends its time on git pulls and full walks. A source that another pass
// re-registered with a path meanwhile must keep its chunks, because a content
// match updated those rows in place and the watermark cannot spare them.
func TestDocsCLI_SweepSkipsASourceThatGainedAPathLos55e(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	docs := t.TempDir()
	if err := os.WriteFile(filepath.Join(docs, "guide.md"), []byte("# Guide\n\nBody prose.\n"), 0644); err != nil {
		t.Fatalf("write guide: %v", err)
	}
	seedDocsDB(t, home, storage.DocSource{Name: "re-registered", Project: "p", Path: docs})

	store := openDocsDB(t, home)
	src, err := store.GetDocSource("re-registered", "p")
	if err != nil || src == nil {
		t.Fatalf("get source: %v (source=%v)", err, src)
	}
	if _, err := store.InsertDocChunk(&storage.DocChunk{
		SourceID:    src.ID,
		SourceFile:  "guide.md",
		SourceHash:  "hash-guide",
		Content:     "Prose of guide.md",
		ContentHash: "content-guide",
	}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	watermark, err := store.MaxDocChunkID()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}

	// The snapshot said "no path"; the row now has one.
	sweepSourceWithoutPath(store, "re-registered", "p", watermark)

	chunks, err := store.GetDocChunksBySource(src.ID)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("the sweep judged a snapshot that the current row no longer matches: the source has a path again")
	}
}

// The interleaving the orchestrator demanded, deterministically: a competing pass
// re-registers the source with a path and re-indexes identical content in the
// window between the state read and the sweep. A content match keeps the old
// chunk ids, so the rows the sweep is about to judge are live. The hook makes the
// interleaving exact instead of racing with a sleep.
func TestDocsCLI_SweepKeepsChunksWhenTheSourceIsReregisteredInTheWindowLos55e(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	docs := t.TempDir()
	if err := os.WriteFile(filepath.Join(docs, "guide.md"), []byte("# Guide\n\nBody prose that stays unchanged.\n"), 0644); err != nil {
		t.Fatalf("write guide: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(home, ".claude", "yesmem"), 0755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	store, err := storage.Open(filepath.Join(home, ".claude", "yesmem", "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := ingest.Config{Project: "p", Name: "reregistered"}
	if _, err := ingest.Run(cfg, []string{docs}, store); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	src, err := store.GetDocSource("reregistered", "p")
	if err != nil || src == nil {
		t.Fatalf("get source: %v (source=%v)", err, src)
	}
	before, err := store.GetDocChunksBySource(src.ID)
	if err != nil || len(before) == 0 {
		t.Fatalf("chunks after the first ingest: %d (err=%v)", len(before), err)
	}

	// The source loses its path; that is the state the CLI sweep is for.
	if _, err := store.UpsertDocSource(&storage.DocSource{Name: "reregistered", Project: "p"}); err != nil {
		t.Fatalf("clear the path: %v", err)
	}
	watermark, err := store.MaxDocChunkID()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}

	var afterReindex []int64
	hook := func() {
		if _, err := store.UpsertDocSource(&storage.DocSource{Name: "reregistered", Project: "p", Path: docs}); err != nil {
			t.Errorf("re-register in the window: %v", err)
			return
		}
		if _, err := ingest.Run(cfg, []string{docs}, store); err != nil {
			t.Errorf("re-index in the window: %v", err)
			return
		}
		chunks, err := store.GetDocChunksBySource(src.ID)
		if err != nil {
			t.Errorf("read chunks after the re-index: %v", err)
			return
		}
		for _, c := range chunks {
			afterReindex = append(afterReindex, c.ID)
		}
	}
	pathReReadHook.Store(&hook)
	t.Cleanup(func() { pathReReadHook.Store(nil) })

	sweepSourceWithoutPath(store, "reregistered", "p", watermark)

	if len(afterReindex) == 0 {
		t.Fatal("the competing pass wrote no chunk, so the window was never exercised")
	}
	// The premise of the bug: a content match keeps the chunk ids, so they are not
	// above the watermark and nothing but the path check can save them.
	for _, id := range afterReindex {
		if id > watermark {
			t.Fatalf("the competing pass wrote a new chunk id %d above the watermark %d: this test no longer covers the content-match case", id, watermark)
		}
	}
	after, err := store.GetDocChunksBySource(src.ID)
	if err != nil {
		t.Fatalf("chunks after the sweep: %v", err)
	}
	if len(after) == 0 {
		t.Fatal("the sweep deleted the chunks of a source that a competing pass had re-registered with a path")
	}
}

// The counter-case: a source whose registered path still exists keeps its
// chunks. A sweep that judges every path-less source must not touch the rest.
func TestDocsCLI_SyncKeepsChunksOfASourceWithALivePathLos55e(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	docs := t.TempDir()
	if err := os.WriteFile(filepath.Join(docs, "guide.md"), []byte("# Guide\n\nBody prose.\n"), 0644); err != nil {
		t.Fatalf("write guide: %v", err)
	}
	seedDocsDB(t, home, storage.DocSource{Name: "live-docs", Project: "p", Path: docs})

	store := openDocsDB(t, home)
	src, err := store.GetDocSource("live-docs", "p")
	if err != nil || src == nil {
		t.Fatalf("get source: %v (source=%v)", err, src)
	}
	if _, err := store.InsertDocChunk(&storage.DocChunk{
		SourceID:    src.ID,
		SourceFile:  "guide.md",
		SourceHash:  "stale-hash",
		Content:     "Older prose of guide.md",
		ContentHash: "stale-content",
	}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	captureOutput(t, func() { runSyncDocs([]string{"--all"}) })

	chunks, err := store.GetDocChunksBySource(src.ID)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("the sync removed the chunks of a source whose path still exists")
	}
}

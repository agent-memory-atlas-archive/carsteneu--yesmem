package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// Two callers reach the same session file in normal operation: the watcher pool
// (which deduplicates only the paths submitted through it) and the startup scan,
// which runs IndexAll straight through tryIndexFile. Both parse the same file,
// both delete and reinsert the session's rows, and the interleaving ends with
// the second one colliding with the first (the unique (session_id, sequence)
// index) or with rows missing from the run. A caller that loses that race hands
// the error to its caller. Repeated rounds: a single round can get lucky.
func TestIndexSessionSerializesConcurrentPassesOnSamePath(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	const sessionID = "s-conc-path"
	const rounds = 10

	for round := 0; round < rounds; round++ {
		path := filepath.Join(dir, fmt.Sprintf("sess-%d.jsonl", round))
		fileContent := concLines(sessionID, round, 0, 200)
		if err := os.WriteFile(path, []byte(fileContent), 0o644); err != nil {
			t.Fatalf("write session: %v", err)
		}

		// Four passes over a file without a watermark: all of them take the full
		// path, as the startup scan and the watcher pool can.
		runConcurrent(t, idx, []string{path, path, path, path}, round, "initial")
		assertContiguousSession(t, store, sessionID, round)
		if got := sessionRowCount(t, store, sessionID); got != 200 {
			t.Fatalf("round %d: %d rows after the initial pass, want 200", round, got)
		}

		// And again while the file is being appended to: all of them see the same
		// watermark and would insert the same tail.
		if err := os.WriteFile(path, []byte(concLines(sessionID, round, 0, 400)), 0o644); err != nil {
			t.Fatalf("grow session: %v", err)
		}
		runConcurrent(t, idx, []string{path, path, path, path}, round, "append")
		assertContiguousSession(t, store, sessionID, round)
		if got := sessionRowCount(t, store, sessionID); got != 400 {
			t.Fatalf("round %d: %d rows after the append pass, want 400", round, got)
		}
	}
}

// Two files can carry the same session id (a resumed session keeps the parent id
// behind a snapshot header, and a restored copy duplicates it). Both take the
// full path, so an interleaved run gives Delete/Delete/Insert/Insert: the second
// insert lands on rows the first one just wrote.
func TestIndexSessionSerializesConcurrentPassesOnSameSessionID(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	const sessionID = "s-conc-session"
	const rounds = 10

	for round := 0; round < rounds; round++ {
		content := concLines(sessionID, round, 0, 60)
		pathA := writeSessionFile(t, dir, fmt.Sprintf("a-%d.jsonl", round), content)
		pathB := writeSessionFile(t, dir, fmt.Sprintf("b-%d.jsonl", round), content)

		runConcurrent(t, idx, []string{pathA, pathB}, round, "same session id")
		assertContiguousSession(t, store, sessionID, round)
		if got := sessionRowCount(t, store, sessionID); got != 60 {
			t.Fatalf("round %d: %d rows, want the 60 of whichever file won the race", round, got)
		}
	}
}

// concLines renders count JSONL lines for a session, starting at index from.
func concLines(sessionID string, round, from, count int) string {
	var sb string
	for i := from; i < from+count; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sb += incLine(sessionID, role, fmt.Sprintf("round %d line %d", round, i), "2026-10-14T10:00:00Z")
	}
	return sb
}

// runConcurrent indexes every path at once and fails on the first error.
func runConcurrent(t *testing.T, idx *Indexer, paths []string, round int, label string) {
	t.Helper()
	start := make(chan struct{})
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			<-start
			errs[i] = idx.IndexSession(p)
		}(i, p)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("round %d (%s): concurrent index %d: %v", round, label, i, err)
		}
	}
}

func sessionRowCount(t *testing.T, store *storage.Store, sessionID string) int {
	t.Helper()
	msgs, err := store.GetMessagesBySession(sessionID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	return len(msgs)
}

// assertContiguousSession checks the invariant the append path depends on: the
// stored rows are one run of sequences starting at zero, with no duplicates.
func assertContiguousSession(t *testing.T, store *storage.Store, sessionID string, round int) {
	t.Helper()
	msgs, err := store.GetMessagesBySession(sessionID)
	if err != nil {
		t.Fatalf("round %d: get messages: %v", round, err)
	}
	if len(msgs) == 0 {
		t.Fatalf("round %d: %q lost all messages", round, sessionID)
	}
	seen := map[int]bool{}
	for _, m := range msgs {
		if seen[m.Sequence] {
			t.Fatalf("round %d: duplicate sequence %d in %q (%d rows)",
				round, m.Sequence, sessionID, len(msgs))
		}
		seen[m.Sequence] = true
	}
	for i := range msgs {
		if msgs[i].Sequence != i {
			t.Fatalf("round %d: %q holds %d rows but sequence %d sits at index %d",
				round, sessionID, len(msgs), msgs[i].Sequence, i)
		}
	}
}

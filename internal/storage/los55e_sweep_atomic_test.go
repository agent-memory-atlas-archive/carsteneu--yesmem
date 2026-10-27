package storage

import (
	"context"
	"database/sql"
	"runtime"
	"strings"
	"testing"
	"time"
)

// YM-374 (Nacharbeit zur Orchestrator-Review): der Sweep einer pfadlosen Quelle
// darf die Pfadpruefung nicht von der Loeschung trennen. Ein paralleler Lauf kann
// die Quelle neu registrieren und neu indizieren, und ein Content-Match behaelt
// die alten Chunk-IDs, also liegen die lebenden Zeilen unter dem Watermark.
//
// Der Test belegt die Atomaritaet direkt: waehrend der Transaktion versucht ein
// zweiter Schreiber dieselbe Quelle zu registrieren und muss an der Schreibsperre
// scheitern. Kein sleep, kein Timing.
func TestSweepPathlessSourceChunksHoldsTheWriteLockAcrossCheckAndDeleteLos55e(t *testing.T) {
	s, path := los38tOpenStore(t)

	srcID, err := s.UpsertDocSource(&DocSource{Name: "pathless", Project: "p"})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	chunkID, err := s.InsertDocChunk(&DocChunk{
		SourceID:    srcID,
		SourceFile:  "guide.md",
		SourceHash:  "hash-guide",
		Content:     "Prose of guide.md",
		ContentHash: "content-guide",
	})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	watermark, err := s.MaxDocChunkID()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}

	other, err := sql.Open("sqlite", sqliteDSN(path, dsnReadWrite, []pragmaSetting{{"busy_timeout", "0"}}))
	if err != nil {
		t.Fatalf("open the competing writer: %v", err)
	}
	other.SetMaxOpenConns(1)
	t.Cleanup(func() { other.Close() })

	var competingErr error
	hook := func() {
		_, competingErr = other.Exec(`UPDATE doc_sources SET path = ? WHERE id = ?`, "/elsewhere", srcID)
	}
	sweepPathlessMidHook.Store(&hook)
	t.Cleanup(func() { sweepPathlessMidHook.Store(nil) })

	chunks, files, learnings, err := s.SweepPathlessSourceChunks(srcID, watermark)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if competingErr == nil {
		t.Fatal("a competing writer acquired the write lock between the path check and the delete: the sweep is not atomic")
	}
	msg := strings.ToLower(competingErr.Error())
	if !strings.Contains(msg, "busy") && !strings.Contains(msg, "locked") {
		t.Fatalf("the competing write failed with %v, want SQLITE_BUSY from the sweep's open transaction", competingErr)
	}
	if chunks != 1 || files != 1 {
		t.Errorf("sweep removed %d chunk(s) in %d document(s), want 1 in 1", chunks, files)
	}
	if len(learnings) != 0 {
		t.Errorf("retired %d learning(s), want 0 for a chunk without learnings", len(learnings))
	}
	if left, err := s.GetDocChunksBySource(srcID); err != nil || len(left) != 0 {
		t.Errorf("chunks after the sweep: %d (err=%v), want 0", len(left), err)
	}
	// The FTS row goes first, while the rowid subquery still resolves. An orphaned
	// FTS row keeps serving a document that no longer exists.
	var ftsRows int
	if err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM doc_chunks_fts WHERE rowid = ?`, chunkID).Scan(&ftsRows); err != nil {
		t.Fatalf("count fts rows: %v", err)
	}
	if ftsRows != 0 {
		t.Errorf("%d full-text row(s) survived the sweep for a deleted chunk", ftsRows)
	}

	// The lock must end with the transaction: the same write succeeds now.
	if _, err := other.Exec(`UPDATE doc_sources SET path = ? WHERE id = ?`, "/elsewhere", srcID); err != nil {
		t.Fatalf("the write lock outlived the sweep transaction: %v", err)
	}
}

// The property the orchestrator's blocker named: the path is read AFTER the
// write lock is taken. A competitor holds the lock with an uncommitted path
// change, the sweep queues on BEGIN IMMEDIATE, the competitor commits, and only
// then does the sweep read the row. A version that reads the row before taking
// the lock would see the old, empty path (an uncommitted change is invisible to
// a reader) and delete live chunks. No sleep: the test waits for the sweep's
// writer connection to be in use, which is one statement before the blocked
// BEGIN.
func TestSweepPathlessSourceChunksReadsThePathUnderTheWriteLockLos55e(t *testing.T) {
	s, path := los38tOpenStore(t)

	srcID, err := s.UpsertDocSource(&DocSource{Name: "locked", Project: "p"})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	if _, err := s.InsertDocChunk(&DocChunk{
		SourceID:    srcID,
		SourceFile:  "guide.md",
		SourceHash:  "hash-guide",
		Content:     "Prose of guide.md",
		ContentHash: "content-guide",
	}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	watermark, err := s.MaxDocChunkID()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}

	ctx := context.Background()
	other, err := sql.Open("sqlite", sqliteDSN(path, dsnReadWrite, []pragmaSetting{{"busy_timeout", "0"}}))
	if err != nil {
		t.Fatalf("open the competing writer: %v", err)
	}
	other.SetMaxOpenConns(1)
	t.Cleanup(func() { other.Close() })
	ocom, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("competing connection: %v", err)
	}
	if _, err := ocom.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("competing BEGIN IMMEDIATE: %v", err)
	}
	if _, err := ocom.ExecContext(ctx, `UPDATE doc_sources SET path = ? WHERE id = ?`, "/elsewhere", srcID); err != nil {
		t.Fatalf("competing update: %v", err)
	}

	type sweepResult struct {
		chunks, files int
		err           error
	}
	done := make(chan sweepResult, 1)
	go func() {
		chunks, files, _, err := s.SweepPathlessSourceChunks(srcID, watermark)
		done <- sweepResult{chunks, files, err}
	}()

	// The sweep holds its writer connection as soon as it calls s.db.Conn, and the
	// next statement is the BEGIN that blocks on the competitor.
	deadline := time.Now().Add(10 * time.Second)
	for s.db.Stats().InUse == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the sweep never acquired its writer connection, so this test proves nothing")
		}
		runtime.Gosched()
	}
	select {
	case early := <-done:
		t.Fatalf("the sweep finished before the competing commit, so nothing was blocked: %+v", early)
	default:
	}
	if _, err := ocom.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("competing commit: %v", err)
	}
	ocom.Close()

	got := <-done
	if got.err != nil {
		t.Fatalf("sweep: %v", got.err)
	}
	if got.chunks != 0 {
		t.Errorf("the sweep deleted %d chunk(s) after the path had been set: it judged a row it read before taking the write lock", got.chunks)
	}
	if left, err := s.GetDocChunksBySource(srcID); err != nil || len(left) != 1 {
		t.Errorf("chunks of a source that gained a path: %d (err=%v), want 1 kept", len(left), err)
	}
}

// The guard itself: a source that has a path keeps its chunks, because the check
// runs on the current row and a pathless caller can hold a stale snapshot.
func TestSweepPathlessSourceChunksIsANoOpForASourceWithAPathLos55e(t *testing.T) {
	s := openTestStore(t)

	srcID, err := s.UpsertDocSource(&DocSource{Name: "registered", Project: "p", Path: "/srv/docs"})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	if _, err := s.InsertDocChunk(&DocChunk{
		SourceID:    srcID,
		SourceFile:  "guide.md",
		SourceHash:  "hash-guide",
		Content:     "Prose of guide.md",
		ContentHash: "content-guide",
	}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	watermark, err := s.MaxDocChunkID()
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}

	chunks, files, learnings, err := s.SweepPathlessSourceChunks(srcID, watermark)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if chunks != 0 || files != 0 || len(learnings) != 0 {
		t.Errorf("sweep touched a source that has a path: chunks=%d files=%d learnings=%d", chunks, files, len(learnings))
	}
	if left, err := s.GetDocChunksBySource(srcID); err != nil || len(left) == 0 {
		t.Errorf("chunks of a registered source after the sweep: %d (err=%v), want them kept", len(left), err)
	}
}

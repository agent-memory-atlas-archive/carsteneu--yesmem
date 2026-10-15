package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/bloom"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// incLine renders one Claude Code JSONL line with array-shaped content, the
// shape both parser branches accept.
func incLine(sessionID, role, text, ts string) string {
	return fmt.Sprintf(`{"type":%q,"sessionId":%q,"cwd":"/proj/incremental","gitBranch":"main",`+
		`"message":{"role":%q,"content":[{"type":"text","text":%q}]},"timestamp":%q}`+"\n",
		role, sessionID, role, text, ts)
}

// writeSessionFile (re)writes a session JSONL and returns its path.
func writeSessionFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	return path
}

func sessionSnapshot(t *testing.T, store *storage.Store, sessionID string) []string {
	t.Helper()
	msgs, err := store.GetMessagesBySession(sessionID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, fmt.Sprintf("%d|%s|%s|%s|%s", m.Sequence, m.SessionID, m.Role, m.MessageType, m.Content))
	}
	return out
}

func mustIndexState(t *testing.T, s *storage.Store, path string) *models.IndexState {
	t.Helper()
	st, err := s.GetIndexState(path)
	if err != nil {
		t.Fatalf("get index state %s: %v", path, err)
	}
	return st
}

// A grown file must be indexed from its watermark: the already stored part is
// untouched, the appended part lands with continuing sequence numbers, and the
// watermark advances to the new end of file.
func TestIndexSessionIndexesOnlyTheAppendedTail(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()

	line1 := incLine("s-inc", "user", "first appended question", "2026-10-14T10:00:01Z")
	line2 := incLine("s-inc", "assistant", "answer to the question", "2026-10-14T10:00:02Z")
	line3 := incLine("s-inc", "user", "third message here", "2026-10-14T10:00:03Z")

	path := writeSessionFile(t, dir, "sess.jsonl", line1)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}

	st := mustIndexState(t, store, path)
	if st.IndexedOffset != int64(len(line1)) {
		t.Errorf("offset after first index: got %d, want %d", st.IndexedOffset, len(line1))
	}
	if st.IndexedSessionID != "s-inc" {
		t.Errorf("session id: got %q, want s-inc", st.IndexedSessionID)
	}
	if st.IndexedMsgCount != 1 {
		t.Errorf("msg count: got %d, want 1", st.IndexedMsgCount)
	}

	writeSessionFile(t, dir, "sess.jsonl", line1+line2)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("append index: %v", err)
	}
	writeSessionFile(t, dir, "sess.jsonl", line1+line2+line3)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("second append index: %v", err)
	}

	got := sessionSnapshot(t, store, "s-inc")
	want := []string{
		"0|s-inc|user|text|first appended question",
		"1|s-inc|assistant|text|answer to the question",
		"2|s-inc|user|text|third message here",
	}
	if len(got) != len(want) {
		t.Fatalf("messages: got %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: got %q, want %q", i, got[i], want[i])
		}
	}

	st = mustIndexState(t, store, path)
	if st.IndexedOffset != int64(len(line1+line2+line3)) {
		t.Errorf("final offset: got %d, want %d", st.IndexedOffset, len(line1+line2+line3))
	}
	if st.IndexedMsgCount != 3 {
		t.Errorf("final msg count: got %d, want 3", st.IndexedMsgCount)
	}

	sess, err := store.GetSession("s-inc")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.MessageCount != 3 {
		t.Errorf("session message_count: got %d, want 3", sess.MessageCount)
	}
	if sess.FirstMessage != "first appended question" {
		t.Errorf("first_message must survive appends: got %q", sess.FirstMessage)
	}
}

// A positive proof that the append fast path is taken: the already-indexed
// messages keep their row ids, which is only true when the session is not
// deleted and reinserted.
func TestIndexSessionAppendKeepsExistingRowIDs(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()

	line1 := incLine("s-ids", "user", "first row stays put", "2026-10-14T10:00:01Z")
	line2 := incLine("s-ids", "assistant", "second row is new", "2026-10-14T10:00:02Z")

	path := writeSessionFile(t, dir, "sess.jsonl", line1)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	first, err := store.GetMessagesBySession("s-ids")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("initial messages: got %d, want 1", len(first))
	}
	firstID := first[0].ID

	writeSessionFile(t, dir, "sess.jsonl", line1+line2)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("append index: %v", err)
	}

	after, err := store.GetMessagesBySession("s-ids")
	if err != nil {
		t.Fatalf("get messages after append: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("messages after append: got %d, want 2", len(after))
	}
	if after[0].ID != firstID {
		t.Errorf("row id changed (%d → %d): the already-indexed part was rewritten, "+
			"so the append fast path was not used", firstID, after[0].ID)
	}
	if after[1].ID == firstID {
		t.Errorf("new message reused the old row id %d", firstID)
	}
}

// Two files can carry the same session id (a resumed session keeps the parent id
// behind a snapshot header). Appending for one of them must not trim messages
// the other contributed, so the fast path refuses a session owned by another
// file and falls back to a full reindex.
func TestIndexSessionRefusesAppendWhenAnotherFileOwnsTheSession(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()

	pathA := filepath.Join(dir, "a.jsonl")
	lineA1 := incLine("s-shared", "user", "owner line one", "2026-10-14T10:00:01Z")
	lineA2 := incLine("s-shared", "assistant", "owner line two", "2026-10-14T10:00:02Z")
	writeSessionFile(t, dir, "a.jsonl", lineA1)
	if err := idx.IndexSession(pathA); err != nil {
		t.Fatalf("index A: %v", err)
	}

	// File B only reveals the session id on its second line, behind a snapshot
	// header — first-wins parsing attributes the whole file to "s-shared".
	pathB := filepath.Join(dir, "b.jsonl")
	snapshot := `{"type":"file-history-snapshot","messageId":"m1"}` + "\n"
	lineB1 := incLine("s-shared", "user", "impostor line one", "2026-10-14T11:00:01Z")
	lineB2 := incLine("s-shared", "assistant", "impostor line two", "2026-10-14T11:00:02Z")
	lineB3 := incLine("s-shared", "user", "impostor line three", "2026-10-14T11:00:03Z")
	writeSessionFile(t, dir, "b.jsonl", snapshot+lineB1+lineB2)
	if err := idx.IndexSession(pathB); err != nil {
		t.Fatalf("index B: %v", err)
	}

	// A grows. Its append must not silently erase B's messages, and vice versa.
	writeSessionFile(t, dir, "a.jsonl", lineA1+lineA2)
	if err := idx.IndexSession(pathA); err != nil {
		t.Fatalf("reindex A: %v", err)
	}

	got := sessionSnapshot(t, store, "s-shared")
	if len(got) != 2 {
		t.Fatalf("session messages: got %v, want A's two lines", got)
	}
	for _, s := range got {
		if strings.Contains(s, "impostor") {
			t.Errorf("cross-file mixture after append: %q", s)
		}
	}
	if got[0] != "0|s-shared|user|text|owner line one" || got[1] != "1|s-shared|assistant|text|owner line two" {
		t.Errorf("session content: %v", got)
	}

	// The same guard applies in the other direction: B's own append is a full
	// reindex, so the session ends up holding B's file content, never a mixture.
	writeSessionFile(t, dir, "b.jsonl", snapshot+lineB1+lineB2+lineB3)
	if err := idx.IndexSession(pathB); err != nil {
		t.Fatalf("reindex B: %v", err)
	}
	got = sessionSnapshot(t, store, "s-shared")
	if len(got) != 3 {
		t.Fatalf("after B grows: got %v, want B's three lines", got)
	}
	for _, s := range got {
		if strings.Contains(s, "owner line") {
			t.Errorf("cross-file mixture after B's append: %q", s)
		}
	}
}

// A larger in-place rewrite is not an append: the stored offset would land in
// the middle of a line and the line would be dropped.
func TestIndexSessionInPlaceRewriteToLargerFileFallsBackToFullReindex(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	first := incLine("s-rewrite", "user", "short original line", "2026-10-14T10:00:01Z")
	writeSessionFile(t, dir, "sess.jsonl", first)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	offset := mustIndexState(t, store, path).IndexedOffset

	longText := strings.Repeat("rewritten content ", 60)
	longLine := incLine("s-rewrite", "user", longText, "2026-10-14T11:00:01Z")
	secondLine := incLine("s-rewrite", "assistant", "second rewritten line", "2026-10-14T11:00:02Z")
	rewritten := longLine + secondLine
	if int(offset) >= len(rewritten) {
		t.Fatalf("test setup: rewritten file (%d bytes) is not longer than the offset %d", len(rewritten), offset)
	}
	if rewritten[offset-1] == '\n' {
		t.Fatalf("test setup: byte %d of the rewritten file is a line boundary", offset-1)
	}

	writeSessionFile(t, dir, "sess.jsonl", rewritten)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("reindex after rewrite: %v", err)
	}

	got := sessionSnapshot(t, store, "s-rewrite")
	if len(got) != 2 {
		t.Fatalf("messages: got %d (%v), want 2", len(got), got)
	}
	if !strings.Contains(got[0], "rewritten content") {
		t.Errorf("first message lost or truncated by a mid-line seek: %q", got[0])
	}
	if got[1] != "1|s-rewrite|assistant|text|second rewritten line" {
		t.Errorf("second message: %q", got[1])
	}
}

// The append path must honour the project exclusion list exactly like the full
// path: a file that carries a watermark from before its project was excluded
// must not keep growing the index.
func TestIndexSessionAppendRespectsProjectExclusion(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	bloomMgr := bloom.New()
	arch := archive.New(filepath.Join(t.TempDir(), "archive"))
	openIdx := New(store, bloomMgr, arch, nil)
	excludedIdx := New(store, bloomMgr, arch, []string{"/proj/incremental"})

	dir := t.TempDir()
	line1 := incLine("s-excluded", "user", "indexed before exclusion", "2026-10-14T10:00:01Z")
	line2 := incLine("s-excluded", "assistant", "added after exclusion", "2026-10-14T10:00:02Z")

	path := writeSessionFile(t, dir, "sess.jsonl", line1)
	if err := openIdx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	before := len(sessionSnapshot(t, store, "s-excluded"))

	writeSessionFile(t, dir, "sess.jsonl", line1+line2)
	if err := excludedIdx.IndexSession(path); err != nil {
		t.Fatalf("index with exclusion: %v", err)
	}

	if after := len(sessionSnapshot(t, store, "s-excluded")); after != before {
		t.Errorf("excluded project gained messages: %d -> %d", before, after)
	}
}

// A session file whose final line has not been terminated yet: the line must be
// indexed (as it always was) and the file must not be left marked as up to date
// with the line missing.
func TestIndexSessionKeepsUnterminatedFinalLine(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	line1 := incLine("s-final", "user", "terminated line", "2026-10-14T10:00:01Z")
	line2 := `{"type":"assistant","sessionId":"s-final","cwd":"/proj/incremental","gitBranch":"main",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"complete json but no newline"}]},` +
		`"timestamp":"2026-10-14T10:00:02Z"}`

	writeSessionFile(t, dir, "sess.jsonl", line1+line2)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index: %v", err)
	}

	got := sessionSnapshot(t, store, "s-final")
	if len(got) != 2 {
		t.Fatalf("messages: got %v, want both lines indexed", got)
	}
	if !strings.Contains(got[1], "complete json but no newline") {
		t.Errorf("final message: %q", got[1])
	}

	// The watermark stops at the newline, with the count that belongs to it, so
	// the unterminated line is re-read once its terminator arrives.
	st := mustIndexState(t, store, path)
	if st.IndexedOffset != int64(len(line1)) {
		t.Errorf("offset: got %d, want %d (last newline)", st.IndexedOffset, len(line1))
	}
	if st.IndexedMsgCount != 1 {
		t.Errorf("msg count: got %d, want 1", st.IndexedMsgCount)
	}

	writeSessionFile(t, dir, "sess.jsonl", line1+line2+"\n")
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index after termination: %v", err)
	}
	got = sessionSnapshot(t, store, "s-final")
	if len(got) != 2 {
		t.Fatalf("after termination: got %v, want 2 without duplicates", got)
	}
	st = mustIndexState(t, store, path)
	if st.IndexedOffset != int64(len(line1+line2)+1) {
		t.Errorf("offset after termination: got %d, want %d", st.IndexedOffset, len(line1+line2)+1)
	}
	if st.IndexedMsgCount != 2 {
		t.Errorf("msg count after termination: got %d, want 2", st.IndexedMsgCount)
	}
}

// An in-place rewrite that happens to leave a newline on the stored offset is
// still a rewrite, not an append: the prefix fingerprint has to catch it.
func TestIndexSessionRewriteWithAlignedBoundaryFallsBackToFullReindex(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	a1 := incLine("s-aligned", "user", "AAAA first version", "2026-10-14T10:00:01Z")
	a2 := incLine("s-aligned", "assistant", "AAAA second version", "2026-10-14T10:00:02Z")
	writeSessionFile(t, dir, "sess.jsonl", a1+a2)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	offset := mustIndexState(t, store, path).IndexedOffset

	// Same line lengths, different content, plus one more line: the stored offset
	// still lands right after a newline, so only the fingerprint distinguishes it.
	t1 := incLine("s-aligned", "user", "BBBB first version", "2026-10-14T11:00:01Z")
	t2 := incLine("s-aligned", "assistant", "BBBB second version", "2026-10-14T11:00:02Z")
	t3 := incLine("s-aligned", "user", "BBBB third version", "2026-10-14T11:00:03Z")
	rewritten := t1 + t2 + t3
	if len(t1) != len(a1) || len(t2) != len(a2) {
		t.Fatalf("test setup: line lengths differ (%d/%d vs %d/%d)", len(t1), len(a1), len(t2), len(a2))
	}
	if rewritten[offset-1] != '\n' {
		t.Fatalf("test setup: byte %d of the rewrite is not a newline", offset-1)
	}
	if int64(len(rewritten)) <= offset {
		t.Fatalf("test setup: rewrite is not larger than the stored offset")
	}

	writeSessionFile(t, dir, "sess.jsonl", rewritten)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("reindex after aligned rewrite: %v", err)
	}

	got := sessionSnapshot(t, store, "s-aligned")
	if len(got) != 3 {
		t.Fatalf("messages: got %v, want the three rewritten lines", got)
	}
	for _, s := range got {
		if strings.Contains(s, "AAAA") {
			t.Errorf("stale prefix survived an in-place rewrite: %q", s)
		}
	}
}

// If the store holds a different number of messages than the watermark records
// (another writer, a binary that predates the watermark, an interrupted pass),
// the append is refused: it could otherwise leave a gap or a duplicate.
func TestIndexSessionRefusesAppendWhenStoreDisagreesWithWatermark(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	line1 := incLine("s-gap", "user", "one gap line", "2026-10-14T10:00:01Z")
	line2 := incLine("s-gap", "assistant", "two gap line", "2026-10-14T10:00:02Z")

	writeSessionFile(t, dir, "sess.jsonl", line1+line2)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}

	// An external writer removed a message behind the watermark's back.
	if err := store.DeleteMessagesFromSequence("s-gap", 1); err != nil {
		t.Fatalf("delete: %v", err)
	}

	line3 := incLine("s-gap", "user", "three gap line", "2026-10-14T10:00:03Z")
	writeSessionFile(t, dir, "sess.jsonl", line1+line2+line3)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("reindex after external change: %v", err)
	}

	got := sessionSnapshot(t, store, "s-gap")
	want := []string{
		"0|s-gap|user|text|one gap line",
		"1|s-gap|assistant|text|two gap line",
		"2|s-gap|user|text|three gap line",
	}
	if len(got) != len(want) {
		t.Fatalf("messages: got %v, want %v (no gap, no duplicate)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: got %q, want %q", i, got[i], want[i])
		}
	}
	st := mustIndexState(t, store, path)
	if st.IndexedMsgCount != 3 {
		t.Errorf("watermark msg count: got %d, want 3", st.IndexedMsgCount)
	}
}

// Stored rows that do not match the watermark's shape (here a gap: N rows, but
// the highest sequence is above N-1) must make the append refuse instead of
// building on top of them. Duplicates — the other non-contiguous shape — can no
// longer be created through the storage API at all: the (session_id, sequence)
// index is UNIQUE (TestInsertMessagesRejectsDuplicateSequence in internal/storage).
func TestIndexSessionRefusesAppendWhenStoredRowsAreNotContiguous(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")

	line1 := incLine("s-gap", "user", "gap base line", "2026-10-14T10:00:01Z")
	line2 := incLine("s-gap", "assistant", "gap second line", "2026-10-14T10:00:02Z")
	writeSessionFile(t, dir, "sess.jsonl", line1+line2)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}

	// A stray row far beyond the watermark leaves the stored run non-contiguous
	// without touching the file.
	stray, err := store.GetMessagesBySession("s-gap")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if err := store.InsertMessages([]models.Message{{
		SessionID:   "s-gap",
		Role:        "user",
		MessageType: "text",
		Content:     "stray line beyond the watermark",
		Timestamp:   stray[len(stray)-1].Timestamp,
		Sequence:    len(stray) + 3,
	}}); err != nil {
		t.Fatalf("insert stray row: %v", err)
	}

	line3 := incLine("s-gap", "user", "third line after the gap", "2026-10-14T10:00:03Z")
	writeSessionFile(t, dir, "sess.jsonl", line1+line2+line3)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	got := sessionSnapshot(t, store, "s-gap")
	want := []string{
		"0|s-gap|user|text|gap base line",
		"1|s-gap|assistant|text|gap second line",
		"2|s-gap|user|text|third line after the gap",
	}
	if len(got) != len(want) {
		t.Fatalf("messages: got %v, want the gap cleaned up and %d rows", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// Incremental indexing must be indistinguishable from a full reindex: same
// stored messages (content, sequence, session) and same full-text hits.
func TestIndexSessionIncrementalMatchesFullReindex(t *testing.T) {
	lines := []string{
		incLine("s-eq", "user", "reference zedtoken alpha", "2026-10-14T10:00:01Z"),
		incLine("s-eq", "assistant", "reply without the marker", "2026-10-14T10:00:02Z"),
		incLine("s-eq", "user", "another zedtoken message", "2026-10-14T10:00:03Z"),
		incLine("s-eq", "assistant", "final zedtoken answer", "2026-10-14T10:00:04Z"),
	}

	// Baseline: one shot over the complete file.
	idxBase, storeBase := setupIndexer(t)
	basePath := writeSessionFile(t, t.TempDir(), "sess.jsonl", lines[0]+lines[1]+lines[2]+lines[3])
	if err := idxBase.IndexSession(basePath); err != nil {
		t.Fatalf("baseline index: %v", err)
	}
	baseSnapshot := sessionSnapshot(t, storeBase, "s-eq")
	baseHits := ftsHitKeys(t, storeBase, "zedtoken")

	// Incremental: same content, delivered in four growing steps, each of which
	// must be handled by the append path (proved by stable row ids).
	idxInc, storeInc := setupIndexer(t)
	incDir := t.TempDir()
	incPath := writeSessionFile(t, incDir, "sess.jsonl", lines[0])
	acc := lines[0]
	var firstRowID int64
	for i := 1; i < len(lines); i++ {
		if err := idxInc.IndexSession(incPath); err != nil {
			t.Fatalf("incremental index step %d: %v", i, err)
		}
		stepMsgs, err := storeInc.GetMessagesBySession("s-eq")
		if err != nil {
			t.Fatalf("get messages at step %d: %v", i, err)
		}
		if len(stepMsgs) == 0 {
			t.Fatalf("no messages stored at step %d", i)
		}
		if firstRowID == 0 {
			firstRowID = stepMsgs[0].ID
		} else if stepMsgs[0].ID != firstRowID {
			t.Fatalf("step %d rewrote the already-indexed part (row id %d → %d): "+
				"the append fast path was not used", i, firstRowID, stepMsgs[0].ID)
		}
		acc += lines[i]
		writeSessionFile(t, incDir, "sess.jsonl", acc)
	}
	if err := idxInc.IndexSession(incPath); err != nil {
		t.Fatalf("incremental final index: %v", err)
	}
	incSnapshot := sessionSnapshot(t, storeInc, "s-eq")
	incHits := ftsHitKeys(t, storeInc, "zedtoken")

	if len(baseSnapshot) != len(incSnapshot) {
		t.Fatalf("snapshot size: baseline %d vs incremental %d\nbase=%v\ninc=%v",
			len(baseSnapshot), len(incSnapshot), baseSnapshot, incSnapshot)
	}
	for i := range baseSnapshot {
		if baseSnapshot[i] != incSnapshot[i] {
			t.Errorf("message %d: baseline %q vs incremental %q", i, baseSnapshot[i], incSnapshot[i])
		}
	}
	if len(baseHits) != len(incHits) {
		t.Fatalf("fts hits: baseline %v vs incremental %v", baseHits, incHits)
	}
	for i := range baseHits {
		if baseHits[i] != incHits[i] {
			t.Errorf("fts hit %d: baseline %q vs incremental %q", i, baseHits[i], incHits[i])
		}
	}
}

func ftsHitKeys(t *testing.T, store *storage.Store, query string) []string {
	t.Helper()
	hits, err := store.SearchMessages(query, 100)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	keys := make([]string, 0, len(hits))
	for _, h := range hits {
		keys = append(keys, fmt.Sprintf("%s#%d", h.SessionID, h.Sequence))
	}
	sort.Strings(keys)
	return keys
}

// A file that shrank was rewritten, not appended to: the old content must not
// survive as stale messages.
func TestIndexSessionShrunkFileFallsBackToFullReindex(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()

	lines := []string{
		incLine("s-shrink", "user", "keepme one", "2026-10-14T10:00:01Z"),
		incLine("s-shrink", "user", "dropme two", "2026-10-14T10:00:02Z"),
		incLine("s-shrink", "user", "dropme three", "2026-10-14T10:00:03Z"),
	}
	path := writeSessionFile(t, dir, "sess.jsonl", lines[0]+lines[1]+lines[2])
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}

	// Rewrite with a shorter prefix (size shrinks).
	rewritten := incLine("s-shrink", "user", "only this remains", "2026-10-14T10:00:04Z")
	writeSessionFile(t, dir, "sess.jsonl", rewritten)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("reindex after shrink: %v", err)
	}

	got := sessionSnapshot(t, store, "s-shrink")
	want := []string{"0|s-shrink|user|text|only this remains"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("after shrink: got %v, want %v", got, want)
	}

	stale, err := store.SearchMessages("dropme", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("stale FTS rows survived the shrink: %d hits", len(stale))
	}
}

// The same file path reused for a different session is a format/identity change:
// the append fast path must not attribute new lines to the old session.
func TestIndexSessionIdentityChangeFallsBackToFullReindex(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()

	oldLine := incLine("s-old", "user", "belongs to the old session", "2026-10-14T10:00:01Z")
	path := writeSessionFile(t, dir, "sess.jsonl", oldLine)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}

	// New content for a different session, deliberately larger than before.
	newContent := incLine("s-new", "user", "belongs to the new session", "2026-10-14T10:01:01Z") +
		incLine("s-new", "assistant", "acknowledged the new session", "2026-10-14T10:01:02Z")
	writeSessionFile(t, dir, "sess.jsonl", newContent)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("reindex after identity change: %v", err)
	}

	newGot := sessionSnapshot(t, store, "s-new")
	if len(newGot) != 2 {
		t.Fatalf("new session messages: got %v, want 2 (full reindex of the new content)", newGot)
	}
	if newGot[0] != "0|s-new|user|text|belongs to the new session" {
		t.Errorf("message 0: %q", newGot[0])
	}
	if newGot[1] != "1|s-new|assistant|text|acknowledged the new session" {
		t.Errorf("message 1: %q", newGot[1])
	}
	for _, s := range newGot {
		if strings.Contains(s, "belongs to the old session") {
			t.Errorf("old content leaked into the new session: %q", s)
		}
	}

	// The previous session keeps its own messages — the full path only replaces
	// the session it is indexing, it never cleans up other session ids.
	oldGot := sessionSnapshot(t, store, "s-old")
	if len(oldGot) != 1 {
		t.Errorf("old session messages: got %v, want its original single message", oldGot)
	}

	st := mustIndexState(t, store, path)
	if st.IndexedSessionID != "s-new" {
		t.Errorf("watermark session id: got %q, want s-new", st.IndexedSessionID)
	}
	if st.IndexedMsgCount != 2 {
		t.Errorf("watermark msg count: got %d, want 2", st.IndexedMsgCount)
	}
}

// An in-flight write (line without trailing newline) must not be indexed; once
// the writer completes it, it appears exactly once.
func TestIndexSessionPartialTrailingLineIsDeferred(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()

	line1 := incLine("s-partial", "user", "complete line", "2026-10-14T10:00:01Z")
	line2 := incLine("s-partial", "assistant", "second complete line", "2026-10-14T10:00:02Z")
	partial := `{"type":"user","sessionId":"s-partial","message":{"role":"user","content":[{"type":"text","text":"half written`

	path := writeSessionFile(t, dir, "sess.jsonl", line1+line2+partial)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index with partial line: %v", err)
	}

	got := sessionSnapshot(t, store, "s-partial")
	if len(got) != 2 {
		t.Fatalf("messages after partial index: got %v, want 2", got)
	}
	st := mustIndexState(t, store, path)
	if st.IndexedOffset != int64(len(line1+line2)) {
		t.Errorf("offset must stop at the last complete line: got %d, want %d",
			st.IndexedOffset, len(line1+line2))
	}

	// Writer finishes the line.
	writeSessionFile(t, dir, "sess.jsonl", line1+line2+incLine("s-partial", "user", "now complete", "2026-10-14T10:00:03Z"))
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index after completion: %v", err)
	}
	got = sessionSnapshot(t, store, "s-partial")
	if len(got) != 3 {
		t.Fatalf("messages after completion: got %v, want 3", got)
	}
	if got[2] != "2|s-partial|user|text|now complete" {
		t.Errorf("third message: got %q", got[2])
	}
}

// Crash safety: if messages were inserted but the watermark never persisted,
// the next run must re-parse the same range and end up with no duplicates.
func TestIndexSessionAppendIsIdempotentAfterLostWatermark(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()

	line1 := incLine("s-idem", "user", "one idem line", "2026-10-14T10:00:01Z")
	line2 := incLine("s-idem", "assistant", "two idem line", "2026-10-14T10:00:02Z")

	path := writeSessionFile(t, dir, "sess.jsonl", line1)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	before := mustIndexState(t, store, path)

	// Successful append of the second line.
	writeSessionFile(t, dir, "sess.jsonl", line1+line2)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("append index: %v", err)
	}
	if got := len(sessionSnapshot(t, store, "s-idem")); got != 2 {
		t.Fatalf("after append: %d messages, want 2", got)
	}

	// Simulate the crash: the insert landed, the watermark update did not.
	if err := store.UpsertIndexState(before); err != nil {
		t.Fatalf("restore stale watermark: %v", err)
	}

	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("replay after lost watermark: %v", err)
	}
	got := sessionSnapshot(t, store, "s-idem")
	if len(got) != 2 {
		t.Fatalf("replay duplicated messages: %v", got)
	}
	if got[0] != "0|s-idem|user|text|one idem line" || got[1] != "1|s-idem|assistant|text|two idem line" {
		t.Errorf("replay produced wrong content: %v", got)
	}

	st := mustIndexState(t, store, path)
	if st.IndexedOffset != int64(len(line1+line2)) {
		t.Errorf("watermark not restored: offset %d, want %d", st.IndexedOffset, len(line1+line2))
	}
}

// The append path must keep bloom-filter coverage for the already-indexed part
// of a session, otherwise cross-session recall regresses.
func TestIndexSessionAppendKeepsBloomCoverage(t *testing.T) {
	idx, _ := setupIndexer(t)
	dir := t.TempDir()

	line1 := incLine("s-bloom", "user", "distinctive alpha marker", "2026-10-14T10:00:01Z")
	line2 := incLine("s-bloom", "user", "later beta marker", "2026-10-14T10:00:02Z")

	path := writeSessionFile(t, dir, "sess.jsonl", line1)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	if got := idx.bloom.MayContain("distinctive"); !containsStr(got, "s-bloom") {
		t.Fatalf("bloom missing initial term: %v", got)
	}

	writeSessionFile(t, dir, "sess.jsonl", line1+line2)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("append index: %v", err)
	}
	if got := idx.bloom.MayContain("distinctive"); !containsStr(got, "s-bloom") {
		t.Errorf("bloom lost coverage of the pre-append part: %v", got)
	}
	if got := idx.bloom.MayContain("beta"); !containsStr(got, "s-bloom") {
		t.Errorf("bloom missing appended term: %v", got)
	}
}

// A session's first index can run before the file carries its first user
// prompt: Claude Code writes header lines (file-history-snapshot, hook and
// attachment entries) before the first turn. The append pass is then the only
// writer that ever sees the prompt, so it has to fill the fields the first
// index left empty — otherwise first_message, the project path and the branch
// stay empty for the life of the session, and everything built on them
// (profiles, narrative, briefing) reads from a half-filled record.
func TestIndexSessionAppendHealsEmptyMetadata(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	const sessionID = "s-heal"

	// Header line only: a session id and a timestamp, no cwd, no branch, no turn.
	header := `{"type":"file-history-snapshot","sessionId":"` + sessionID + `","timestamp":"2026-10-14T10:00:00Z"}` + "\n"
	path := writeSessionFile(t, dir, "sess.jsonl", header)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index header-only file: %v", err)
	}

	stored, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.FirstMessage != "" || stored.Project != "" {
		t.Fatalf("precondition: expected empty metadata after the header-only pass, got first=%q project=%q",
			stored.FirstMessage, stored.Project)
	}

	// The file grows: the first turn arrives, with the context the header lacked.
	writeSessionFile(t, dir, "sess.jsonl", header+incLine(sessionID, "user", "the first real prompt", "2026-10-14T10:05:00Z"))
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("append index: %v", err)
	}

	got, err := store.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.FirstMessage != "the first real prompt" {
		t.Errorf("first_message: got %q, want %q", got.FirstMessage, "the first real prompt")
	}
	if got.Project != "/proj/incremental" || got.ProjectShort != "/proj/incremental" {
		t.Errorf("project: got %q/%q, want /proj/incremental", got.Project, got.ProjectShort)
	}
	if got.GitBranch != "main" {
		t.Errorf("git_branch: got %q, want main", got.GitBranch)
	}
	// started_at is already set from the header line and must not be replaced by
	// the tail's first timestamp — the append knows when the session started.
	wantStarted := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	if !got.StartedAt.Equal(wantStarted) {
		t.Errorf("started_at: got %s, want %s", got.StartedAt, wantStarted)
	}
}

// A subagent's agent type is guessed from its first user message. When the
// first index ran before that message existed, the stored guess came from an
// empty string and would keep the session filed as a generic agent forever.
func TestIndexSessionAppendReguessesSubagentType(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	const agentID = "agent-heal"

	header := `{"type":"file-history-snapshot","sessionId":"s-parent","agentId":"` + agentID + `","timestamp":"2026-10-14T10:00:00Z"}` + "\n"
	path := writeSessionFile(t, dir, "sess.jsonl", header)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index header-only file: %v", err)
	}

	stored, err := store.GetSession(agentID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.AgentType != "general-purpose" {
		t.Fatalf("precondition: agent_type from an empty first message: got %q, want general-purpose", stored.AgentType)
	}

	writeSessionFile(t, dir, "sess.jsonl", header+
		incLine(agentID, "user", "please review this patch for races", "2026-10-14T10:05:00Z"))
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("append index: %v", err)
	}

	got, err := store.GetSession(agentID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.AgentType != "code-reviewer" {
		t.Errorf("agent_type: got %q, want code-reviewer", got.AgentType)
	}
	if got.ParentSessionID != "s-parent" {
		t.Errorf("parent_session_id: got %q, want s-parent", got.ParentSessionID)
	}
}

// A line the writer has not terminated yet is deferred. The pass that defers it
// must still record what it has seen: it stores no messages, but leaving the
// stored file size behind makes every later scan re-read the file for nothing,
// at once per session and forever.
func TestIndexSessionDeferredFragmentDoesNotForceRepeatedReindex(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	const sessionID = "s-frag"

	line1 := incLine(sessionID, "user", "complete line", "2026-10-14T10:00:01Z")
	// A line the writer is still working on: first an incomplete JSON value, then
	// the closed value, and only then the terminator.
	fragmentOpen := `{"type":"assistant","sessionId":"` + sessionID + `","cwd":"/proj/incremental","gitBranch":"main",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"still writ`
	fragmentClosed := fragmentOpen + `ing"}]},"timestamp":"2026-10-14T10:00:02Z"}`

	path := writeSessionFile(t, dir, "sess.jsonl", line1+fragmentOpen)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	if st := mustIndexState(t, store, path); st.IndexedOffset != int64(len(line1)) {
		t.Fatalf("offset must stop at the last complete line: got %d, want %d", st.IndexedOffset, len(line1))
	}

	// The writer appends the rest of the value; the line is still unterminated,
	// so this pass has nothing to store.
	writeSessionFile(t, dir, "sess.jsonl", line1+fragmentClosed)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index with deferred fragment: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if store.NeedsReindex(path, info.Size(), info.ModTime()) {
		t.Error("deferred fragment left the file marked as needing reindex, so every scan re-reads it")
	}
	if st := mustIndexState(t, store, path); st.IndexedOffset != int64(len(line1)) {
		t.Errorf("offset must not advance past an unterminated line: got %d, want %d", st.IndexedOffset, len(line1))
	}
	if got := sessionSnapshot(t, store, sessionID); len(got) != 1 {
		t.Errorf("deferred line must not be stored: got %v, want just the complete line", got)
	}

	// Once the terminator arrives the line lands exactly once.
	writeSessionFile(t, dir, "sess.jsonl", line1+fragmentClosed+"\n")
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index after completion: %v", err)
	}
	got := sessionSnapshot(t, store, sessionID)
	want := []string{
		"0|" + sessionID + "|user|text|complete line",
		"1|" + sessionID + "|assistant|text|still writing",
	}
	if len(got) != len(want) {
		t.Fatalf("messages after completion: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// A session file can carry a different session id on its first line than on its
// message lines: the first line with an id wins (parser.go:104-106) while every
// message keeps its own line's id. Two of 5962 files in a real corpus do this,
// and the store's delete is keyed on the session's id — so the rows the pass
// inserts land under another id. Before the unique index that meant the same rows
// were re-inserted on every pass (one live session reached 42097 rows for 473
// sequences); with it, the insert is rejected and the pass can never succeed
// again, because the watermark is never written and NeedsReindex stays true. Every
// row therefore has to carry the session's own id, which is what the append path
// already does for its tail and what the full path already does for subagents.
func TestIndexSessionRekeysMessagesOfAMixedSessionIDFile(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()

	// The header names the session; the turn lines name a different id.
	header := `{"type":"file-history-snapshot","sessionId":"s-outer","cwd":"/proj/mixed","timestamp":"2026-10-14T10:00:00Z"}` + "\n"
	line1 := incLine("s-inner", "user", "mixed first line", "2026-10-14T10:00:01Z")
	line2 := incLine("s-inner", "assistant", "mixed second line", "2026-10-14T10:00:02Z")
	line3 := incLine("s-inner", "user", "mixed third line", "2026-10-14T10:00:03Z")
	path := writeSessionFile(t, dir, "mixed.jsonl", header+line1+line2)

	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("first index: %v", err)
	}
	// The file grows, so the next pass re-reads it — as the start scan does after
	// every daemon restart.
	writeSessionFile(t, dir, "mixed.jsonl", header+line1+line2+line3)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("second index: %v", err)
	}

	got := sessionSnapshot(t, store, "s-outer")
	want := []string{
		"0|s-outer|user|text|mixed first line",
		"1|s-outer|assistant|text|mixed second line",
		"2|s-outer|user|text|mixed third line",
	}
	if len(got) != len(want) {
		t.Fatalf("messages under the session's id: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// The append can be the first pass that learns a session's project: the header
// lines that precede the first turn carry no working directory. When the
// exclusion list names that project, the append has to stop there, exactly like
// the full path — which reads the project from its own parse rather than from the
// store — or an excluded project's messages would be indexed once.
func TestIndexSessionAppendHonoursExclusionLearnedFromTheTail(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	const excluded = "/proj/excluded"
	idx := New(store, bloom.New(), archive.New(filepath.Join(t.TempDir(), "archive")), []string{excluded})

	dir := t.TempDir()
	header := `{"type":"file-history-snapshot","sessionId":"s-excl","timestamp":"2026-10-14T10:00:00Z"}` + "\n"
	path := writeSessionFile(t, dir, "sess.jsonl", header)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("index header-only file: %v", err)
	}

	// The tail names the excluded project for the first time.
	excludedLine := `{"type":"user","sessionId":"s-excl","cwd":"` + excluded + `",` +
		`"message":{"role":"user","content":[{"type":"text","text":"hidden turn"}]},` +
		`"timestamp":"2026-10-14T10:05:00Z"}` + "\n"
	writeSessionFile(t, dir, "sess.jsonl", header+excludedLine)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("append index: %v", err)
	}

	if got := sessionSnapshot(t, store, "s-excl"); len(got) != 0 {
		t.Errorf("messages of an excluded project: got %v, want none", got)
	}
}

// A session that was previously indexed from another file leaves that file's
// index_state row behind when the file is moved or deleted. The row must not
// keep the session's new file out of the append fast path forever: it describes
// a path that holds nothing, while the file in front of us may be the only copy
// left. The row is dropped rather than ignored, so the recorded state stays
// honest.
func TestIndexSessionAppendIgnoresIndexStateOfMissingFile(t *testing.T) {
	idx, store := setupIndexer(t)
	dir := t.TempDir()
	const sessionID = "s-moved"

	line1 := incLine(sessionID, "user", "first moved line", "2026-10-14T10:00:01Z")
	line2 := incLine(sessionID, "assistant", "second moved line", "2026-10-14T10:00:02Z")
	path := writeSessionFile(t, dir, "moved.jsonl", line1)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	before, err := store.GetMessagesBySession(sessionID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("initial messages: got %d, want 1", len(before))
	}

	// The session also carries an index_state row for a file that is gone.
	if err := store.UpsertIndexState(&models.IndexState{
		JSONLPath:        filepath.Join(dir, "gone.jsonl"),
		FileSize:         4096,
		FileMtime:        time.Now(),
		IndexedAt:        time.Now(),
		IndexedOffset:    4096,
		IndexedSessionID: sessionID,
		IndexedMsgCount:  7,
	}); err != nil {
		t.Fatalf("seed stale owner row: %v", err)
	}

	writeSessionFile(t, dir, "moved.jsonl", line1+line2)
	if err := idx.IndexSession(path); err != nil {
		t.Fatalf("append index: %v", err)
	}

	after, err := store.GetMessagesBySession(sessionID)
	if err != nil {
		t.Fatalf("get messages after append: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("messages after append: got %d, want 2", len(after))
	}
	if after[0].ID != before[0].ID {
		t.Errorf("row id changed (%d → %d): the row of a file that no longer exists "+
			"forced a full reindex", before[0].ID, after[0].ID)
	}
	// And the stale row is gone: it owns nothing.
	if _, err := store.GetIndexState(filepath.Join(dir, "gone.jsonl")); err == nil {
		t.Error("the index_state row of the missing file survived, so the session keeps a claim that holds nothing")
	}
}

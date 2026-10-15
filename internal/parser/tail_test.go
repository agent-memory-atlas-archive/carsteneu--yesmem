package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// claudeLine builds a minimal Claude Code JSONL line with the given content.
// Content is emitted as a content-block array, which is the shape both the
// user and the assistant branch of the parser understand.
func claudeLine(sessionID, role, text, ts string) string {
	return `{"type":"` + role + `","sessionId":"` + sessionID + `","cwd":"/proj","gitBranch":"main",` +
		`"message":{"role":"` + role + `","content":[{"type":"text","text":"` + text + `"}]},` +
		`"timestamp":"` + ts + `"}` + "\n"
}

func codexJSONLine(typ, payload, ts string) string {
	return `{"timestamp":"` + ts + `","type":"` + typ + `","payload":` + payload + `}` + "\n"
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l
	}
	return out
}

func writeCodexFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, ".codex", "sessions", "2026", "10", "14", "sess.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// A trailing line without a newline is an in-flight write: it must be neither
// parsed nor counted into the offset, so the next pass can pick it up whole.
func TestParseAutoTailLeavesIncompleteTrailingLine(t *testing.T) {
	first := claudeLine("s-tail", "user", "msg one", "2026-10-14T10:00:01Z")
	second := claudeLine("s-tail", "assistant", "msg two", "2026-10-14T10:00:02Z")
	partial := `{"type":"user","sessionId":"s-tail","message":{"role":"user","content":"msg three"`

	path := writeFile(t, "sess.jsonl", first+second+partial)

	res, err := ParseAutoTail(path, 0, 0)
	if err != nil {
		t.Fatalf("tail parse: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("messages: got %d, want 2 (partial line must not be parsed)", len(res.Messages))
	}
	if want := int64(len(first + second)); res.NewOffset != want {
		t.Errorf("NewOffset: got %d, want %d", res.NewOffset, want)
	}

	// Writer finishes the line: the same offset now yields exactly the rest.
	third := claudeLine("s-tail", "user", "msg three", "2026-10-14T10:00:03Z")
	full := first + second + third
	if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	res2, err := ParseAutoTail(path, res.NewOffset, 2)
	if err != nil {
		t.Fatalf("tail parse 2: %v", err)
	}
	if len(res2.Messages) != 1 {
		t.Fatalf("second tail: got %d messages, want 1", len(res2.Messages))
	}
	if res2.Messages[0].Content != "msg three" {
		t.Errorf("content: got %q", res2.Messages[0].Content)
	}
	if res2.Messages[0].Sequence != 2 {
		t.Errorf("sequence: got %d, want 2 (baseSeq continuation)", res2.Messages[0].Sequence)
	}
	if res2.NewOffset != int64(len(full)) {
		t.Errorf("NewOffset: got %d, want %d", res2.NewOffset, len(full))
	}
}

// Repeated tail parses from the same offset must be stable (crash retry).
func TestParseAutoTailIsRepeatable(t *testing.T) {
	content := claudeLine("s-rep", "user", "one", "2026-10-14T10:00:01Z") +
		claudeLine("s-rep", "assistant", "two", "2026-10-14T10:00:02Z") +
		claudeLine("s-rep", "user", "three", "2026-10-14T10:00:03Z")
	path := writeFile(t, "sess.jsonl", content)
	split := int64(len(claudeLine("s-rep", "user", "one", "2026-10-14T10:00:01Z")))

	a, err := ParseAutoTail(path, split, 1)
	if err != nil {
		t.Fatalf("tail parse: %v", err)
	}
	b, err := ParseAutoTail(path, split, 1)
	if err != nil {
		t.Fatalf("tail parse repeat: %v", err)
	}
	if len(a.Messages) != len(b.Messages) || a.NewOffset != b.NewOffset {
		t.Fatalf("not repeatable: %d/%d msgs, offsets %d/%d",
			len(a.Messages), len(b.Messages), a.NewOffset, b.NewOffset)
	}
	for i := range a.Messages {
		if a.Messages[i].Content != b.Messages[i].Content || a.Messages[i].Sequence != b.Messages[i].Sequence {
			t.Errorf("message %d differs between runs", i)
		}
	}
}

// The concatenation of tail parses must equal one full parse: same messages,
// same sequences, same metadata-derived fields.
func TestParseAutoTailEqualsFullParseClaude(t *testing.T) {
	lines := []string{
		claudeLine("s-eq", "user", "first question", "2026-10-14T10:00:01Z"),
		claudeLine("s-eq", "assistant", "first answer", "2026-10-14T10:00:02Z"),
		claudeLine("s-eq", "user", "second question", "2026-10-14T10:00:03Z"),
		claudeLine("s-eq", "assistant", "second answer", "2026-10-14T10:00:04Z"),
	}
	path := writeFile(t, "sess.jsonl", lines[0])

	full, _, err := ParseSessionFile(path)
	if err != nil {
		t.Fatalf("full parse (1 line): %v", err)
	}
	var incremental []string
	for _, m := range full {
		incremental = append(incremental, m.Content)
	}
	offset := int64(len(lines[0]))

	for i := 1; i < len(lines); i++ {
		if err := os.WriteFile(path, []byte(joinLines(lines[:i+1])), 0o644); err != nil {
			t.Fatalf("write step %d: %v", i, err)
		}
		res, err := ParseAutoTail(path, offset, len(incremental))
		if err != nil {
			t.Fatalf("tail step %d: %v", i, err)
		}
		offset = res.NewOffset
		for _, m := range res.Messages {
			if m.Sequence != len(incremental) {
				t.Fatalf("step %d: sequence %d, want %d", i, m.Sequence, len(incremental))
			}
			incremental = append(incremental, m.Content)
		}
	}

	fullAll, _, err := ParseSessionFile(path)
	if err != nil {
		t.Fatalf("full parse: %v", err)
	}
	if len(fullAll) != len(incremental) {
		t.Fatalf("count: full=%d incremental=%d", len(fullAll), len(incremental))
	}
	for i := range fullAll {
		if fullAll[i].Content != incremental[i] {
			t.Errorf("message %d: full %q vs incremental %q", i, fullAll[i].Content, incremental[i])
		}
	}
}

func TestParseAutoTailEqualsFullParseCodex(t *testing.T) {
	head := codexJSONLine("session_meta", `{"id":"codex-sess-1","timestamp":"2026-10-14T10:00:00Z","cwd":"/proj"}`,
		"2026-10-14T10:00:00Z")
	second := codexJSONLine("response_item", `{"type":"message","role":"user","content":"hello codex"}`,
		"2026-10-14T10:00:01Z")
	third := codexJSONLine("response_item", `{"type":"message","role":"assistant","content":"hi back"}`,
		"2026-10-14T10:00:02Z")

	path := writeCodexFile(t, head)

	res1, err := ParseAutoTail(path, 0, 0)
	if err != nil {
		t.Fatalf("tail parse head: %v", err)
	}
	if len(res1.Messages) != 0 {
		t.Fatalf("head-only tail: got %d messages, want 0", len(res1.Messages))
	}
	if res1.NewOffset != int64(len(head)) {
		t.Fatalf("offset: got %d, want %d", res1.NewOffset, len(head))
	}

	if err := os.WriteFile(path, []byte(head+second), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res2, err := ParseAutoTail(path, res1.NewOffset, 0)
	if err != nil {
		t.Fatalf("tail parse 2: %v", err)
	}
	if len(res2.Messages) != 1 || res2.Messages[0].Content != "hello codex" {
		t.Fatalf("second tail: %+v", res2.Messages)
	}
	if res2.Messages[0].Sequence != 0 {
		t.Errorf("sequence: got %d, want 0", res2.Messages[0].Sequence)
	}

	if err := os.WriteFile(path, []byte(head+second+third), 0o644); err != nil {
		t.Fatalf("write 3: %v", err)
	}
	res3, err := ParseAutoTail(path, res2.NewOffset, 1)
	if err != nil {
		t.Fatalf("tail parse 3: %v", err)
	}
	if len(res3.Messages) != 1 || res3.Messages[0].Content != "hi back" {
		t.Fatalf("third tail: %+v", res3.Messages)
	}

	full, _, err := ParseCodexSession(path)
	if err != nil {
		t.Fatalf("full codex parse: %v", err)
	}
	if len(full) != 2 {
		t.Fatalf("full parse: got %d messages, want 2", len(full))
	}
	if full[0].Content != "hello codex" || full[1].Content != "hi back" {
		t.Errorf("full parse content mismatch: %+v", full)
	}
}

func TestFileSessionIdentityClaude(t *testing.T) {
	id, err := FileSessionIdentity("testdata/sample-session.jsonl")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id != "test-session-001" {
		t.Errorf("identity: got %q, want test-session-001", id)
	}
}

// Subagent files carry the parent sessionId plus their own agentId; the agent
// id is the identity yesmem stores for them.
func TestFileSessionIdentitySubagentPrefersAgentID(t *testing.T) {
	id, err := FileSessionIdentity("testdata/sample-subagent.jsonl")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id != "abc123def" {
		t.Errorf("identity: got %q, want abc123def", id)
	}
}

func TestFileSessionIdentityCodex(t *testing.T) {
	path := writeCodexFile(t, codexJSONLine("session_meta",
		`{"id":"codex-abc","timestamp":"2026-10-14T10:00:00Z","cwd":"/proj"}`, "2026-10-14T10:00:00Z"))
	id, err := FileSessionIdentity(path)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id != "codex-abc" {
		t.Errorf("identity: got %q, want codex-abc", id)
	}
}

func TestFileSessionIdentityUnknownHead(t *testing.T) {
	path := writeFile(t, "empty.jsonl", "")
	id, err := FileSessionIdentity(path)
	if err != nil {
		t.Fatalf("identity on empty file: %v", err)
	}
	if id != "" {
		t.Errorf("identity: got %q, want empty (unknown)", id)
	}
}

// Claude Code writes file-history-snapshot header lines before the first message,
// so the identity is not necessarily on line 1. Missing it would silently keep
// the whole file on the O(file) reindex path.
func TestFileSessionIdentitySkipsHeaderLines(t *testing.T) {
	snapshot := `{"type":"file-history-snapshot","messageId":"m1","snapshot":{"trackedFileBackups":{}}}` + "\n"
	content := snapshot +
		`{"type":"file-history-snapshot","messageId":"m2","snapshot":{"trackedFileBackups":{}}}` + "\n" +
		claudeLine("s-late", "user", "first real message", "2026-10-14T10:00:01Z")
	path := writeFile(t, "sess.jsonl", content)

	id, err := FileSessionIdentity(path)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id != "s-late" {
		t.Errorf("identity: got %q, want s-late", id)
	}
}

// A subagent id anywhere in the header block wins over the parent session id
// that every line of such a file also carries.
func TestFileSessionIdentityPrefersAgentIDBeyondLineOne(t *testing.T) {
	content := `{"parentUuid":null,"sessionId":"parent-session","cwd":"/p","type":"user","message":{"role":"user","content":"hi"},"timestamp":"2026-10-14T10:00:00Z"}` + "\n" +
		`{"parentUuid":"u1","sessionId":"parent-session","agentId":"agent-late","cwd":"/p","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"yo"}]},"timestamp":"2026-10-14T10:00:01Z"}` + "\n"
	path := writeFile(t, "subagent.jsonl", content)

	id, err := FileSessionIdentity(path)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id != "agent-late" {
		t.Errorf("identity: got %q, want agent-late", id)
	}
}

// CRLF-terminated lines must be byte-accounted correctly, otherwise the stored
// offset drifts and the tail is re-read from the middle of a line.
func TestParseAutoTailHandlesCRLF(t *testing.T) {
	l1 := claudeLine("s-crlf", "user", "first crlf line", "2026-10-14T10:00:01Z")
	l2 := claudeLine("s-crlf", "assistant", "second crlf line", "2026-10-14T10:00:02Z")
	l3 := claudeLine("s-crlf", "user", "third crlf line", "2026-10-14T10:00:03Z")
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

	path := writeFile(t, "sess.jsonl", crlf(l1)+crlf(l2)+crlf(l3))

	res, err := ParseAutoTail(path, 0, 0)
	if err != nil {
		t.Fatalf("tail parse: %v", err)
	}
	if len(res.Messages) != 3 {
		t.Fatalf("messages: got %d, want 3", len(res.Messages))
	}
	wantOffset := int64(len(crlf(l1) + crlf(l2) + crlf(l3)))
	if res.NewOffset != wantOffset {
		t.Errorf("NewOffset: got %d, want %d", res.NewOffset, wantOffset)
	}

	// Re-reading from the exact boundary must find nothing new.
	res2, err := ParseAutoTail(path, res.NewOffset, 3)
	if err != nil {
		t.Fatalf("second tail parse: %v", err)
	}
	if len(res2.Messages) != 0 {
		t.Errorf("second parse found %d messages, want 0 (offset drift)", len(res2.Messages))
	}
}

// The historical whole-file parser accepted a final line that had no trailing
// newline; that must not regress, because the line would be missing from search
// and from extraction. An append, by contrast, must not consume such a line: the
// writer may still extend it. The watermark therefore stops at the last newline
// and records the message count belonging to that position.
func TestParseSessionFileAcceptsUnterminatedFinalLine(t *testing.T) {
	complete := claudeLine("s-final", "user", "complete line", "2026-10-14T10:00:01Z")
	unterminated := `{"type":"user","sessionId":"s-final","cwd":"/p","message":{"role":"user","content":[{"type":"text","text":"complete json, no newline"}]},"timestamp":"2026-10-14T10:00:02Z"}`

	path := writeFile(t, "sess.jsonl", complete+unterminated)
	msgs, _, err := ParseSessionFile(path)
	if err != nil {
		t.Fatalf("full parse: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages: got %d, want 2 (the historical parser kept the final line)", len(msgs))
	}
	if msgs[1].Content != "complete json, no newline" {
		t.Errorf("final message content: %q", msgs[1].Content)
	}

	// The append parser must not consume it, and must report the watermark at the
	// last newline with the message count that belongs to it.
	res, err := ParseAutoTail(path, 0, 0)
	if err != nil {
		t.Fatalf("whole-file tail parse: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Errorf("offset-0 tail parse: got %d messages, want 2", len(res.Messages))
	}
	if want := int64(len(complete)); res.NewOffset != want {
		t.Errorf("NewOffset: got %d, want %d (last newline)", res.NewOffset, want)
	}
	if res.CompleteCount != 1 {
		t.Errorf("CompleteCount: got %d, want 1", res.CompleteCount)
	}

	res2, err := ParseAutoTail(path, int64(len(complete)), 1)
	if err != nil {
		t.Fatalf("append parse: %v", err)
	}
	if len(res2.Messages) != 0 {
		t.Errorf("append parse consumed an unterminated line: %d messages", len(res2.Messages))
	}
	if res2.NewOffset != int64(len(complete)) {
		t.Errorf("append NewOffset: got %d, want it unchanged at %d", res2.NewOffset, len(complete))
	}
}

// An offset beyond EOF (file shrank) must not panic or return bogus messages,
// and must not advance — the caller then falls back to a full reindex.
func TestParseAutoTailOffsetBeyondEOF(t *testing.T) {
	path := writeFile(t, "sess.jsonl", claudeLine("s-x", "user", "only", "2026-10-14T10:00:01Z"))
	res, err := ParseAutoTail(path, 999999, 7)
	if err != nil {
		t.Fatalf("tail parse: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("messages: got %d, want 0", len(res.Messages))
	}
	if res.NewOffset != 999999 {
		t.Errorf("NewOffset: got %d, want 999999 (no progress past EOF)", res.NewOffset)
	}
}

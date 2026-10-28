package storage

import "testing"

// los38yInsertBlobOnlyMessage recreates the row shape that a blob backfill
// produces: the FTS row carries the thinking text, the content column is empty and
// the body sits in content_blob. On the live database (20.10.2026) 115883 rows
// have that body shape and none of them is indexed in messages_fts yet, so search
// cannot return them at all; the fallback below is what makes such a row render
// its text once it is indexed (restored legacy backups and a future backfill).
func los38yInsertBlobOnlyMessage(t *testing.T, s *Store) int64 {
	t.Helper()
	const body = "the thinking body of the message"
	res, err := s.messagesWriteDB().Exec(`INSERT INTO messages
		(session_id, source_agent, role, message_type, content, content_blob, tool_name, file_path, timestamp, sequence, model)
		VALUES ('ses-blob38y', 'claude', 'assistant', 'thinking', '', ?, '', '', '2026-10-20T17:00:00Z', 1, '')`,
		[]byte(body))
	if err != nil {
		t.Fatalf("insert blob-only message: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	if _, err := s.messagesWriteDB().Exec(`INSERT INTO messages_fts(rowid, content) VALUES (?, ?)`, id, body); err != nil {
		t.Fatalf("insert fts row: %v", err)
	}
	return id
}

// TestSearchSnippetFallsBackToContentBlobLos38y covers YM-369: a message whose
// body lives only in content_blob must not surface as a hit with an empty snippet.
func TestSearchSnippetFallsBackToContentBlobLos38y(t *testing.T) {
	s := mustOpen(t)
	los38yInsertBlobOnlyMessage(t, s)

	hits, err := s.SearchMessagesCtx("thinking", "", "", 10)
	if err != nil {
		t.Fatalf("search messages: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if want := "the thinking body of the message"; hits[0].Content != want {
		t.Fatalf("snippet = %q, want %q (content_blob body)", hits[0].Content, want)
	}
}

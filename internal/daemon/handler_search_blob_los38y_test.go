package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestDeepSearchContextFallsBackToContentBlobLos38y covers the reachable half of
// YM-369: deep_search renders a ±3 context window, and a neighbour whose body sits
// only in content_blob (115883 such rows live on 20.10.2026, none of them indexed
// in messages_fts) used to render as an empty line.
func TestDeepSearchContextFallsBackToContentBlobLos38y(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().Format("2006-01-02 15:04:05")
	db := s.DB()
	if _, err := db.Exec(`INSERT INTO messages(session_id, role, message_type, content, timestamp, sequence)
		VALUES ('ses-blob-ctx', 'assistant', 'text', 'needle in the haystack', ?, 1)`, now); err != nil {
		t.Fatalf("insert hit: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages(session_id, role, message_type, content, content_blob, timestamp, sequence)
		VALUES ('ses-blob-ctx', 'assistant', 'thinking', '', ?, ?, 2)`, []byte("neighbour thinking body"), now); err != nil {
		t.Fatalf("insert blob-only neighbour: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages_fts(rowid, content) SELECT id, content FROM messages WHERE content <> ''`); err != nil {
		t.Fatalf("sync fts: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id, project, project_short, message_count, jsonl_path, started_at, indexed_at) VALUES ('ses-blob-ctx', '/test', 'test', 2, '', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	resp := h.Handle(Request{
		Method: "deep_search",
		Params: map[string]any{"query": "needle haystack", "limit": float64(5)},
	})
	if resp.Error != "" {
		t.Fatalf("deep_search error: %s", resp.Error)
	}

	var results []map[string]any
	b, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(b, &results); err != nil {
		t.Fatalf("decode results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	context, _ := results[0]["context"].(string)
	if !strings.Contains(context, "neighbour thinking body") {
		t.Fatalf("context %q does not carry the blob-only neighbour body", context)
	}
}

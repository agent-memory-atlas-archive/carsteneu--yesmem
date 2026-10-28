package storage

import (
	"testing"
	"time"
)

// los56eDocChunk writes one chunk and forces its created_at to the given SQLite
// text, so the test controls the stored spelling instead of the wall clock.
func los56eDocChunk(t *testing.T, s *Store, sourceID int64, content, createdAt string) int64 {
	t.Helper()
	id, err := s.InsertDocChunk(&DocChunk{
		SourceID:   sourceID,
		SourceFile: "doc.md",
		SourceHash: "hash-" + content,
		Content:    content,
		Metadata:   map[string]string{"k": content},
	})
	if err != nil {
		t.Fatalf("insert doc chunk %q: %v", content, err)
	}
	if _, err := s.DB().Exec(`UPDATE doc_chunks SET created_at = ? WHERE id = ?`, createdAt, id); err != nil {
		t.Fatalf("stamp doc chunk %q: %v", content, err)
	}
	return id
}

// TestLos56eDocsSearchWindowUsesInstants is the regression test for YM-308 (1):
// SearchDocChunksFTS compared since/before as raw text against
// doc_chunks.created_at. The column carries SQLite's CURRENT_TIMESTAMP form
// ("2026-10-20 21:00:00", UTC), while the documented bound forms include
// RFC3339 with an offset ("2026-10-20T00:00:00+02:00"). The space separator
// sorts below the 'T', so a since bound in that form dropped the whole day, and
// a before bound inside the same day kept chunks written after it.
func TestLos56eDocsSearchWindowUsesInstants(t *testing.T) {
	s := mustOpen(t)
	mustUpsertDocSource(t, s, &DocSource{Name: "los56e-window", Project: ""})
	ds, err := s.GetDocSource("los56e-window", "")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}

	// 21:00 UTC on 2026-10-20, stored the way the schema default writes it.
	los56eDocChunk(t, s, ds.ID, "doc evening", "2026-10-20 21:00:00")
	// 09:00 UTC, before the midday bound below.
	los56eDocChunk(t, s, ds.ID, "doc morning", "2026-10-20 09:00:00")

	cases := []struct {
		name   string
		since  string
		before string
		want   []string
		why    string
	}{
		{
			name:  "since in RFC3339 with offset keeps the same day",
			since: "2026-10-20T00:00:00+02:00",
			want:  []string{"doc evening", "doc morning"},
			why:   "every chunk of 2026-10-20 lies after the start of that day",
		},
		{
			name:   "before inside the day drops later chunks",
			before: "2026-10-20T12:00:00+00:00",
			want:   []string{"doc morning"},
			why:    "21:00 UTC is after the midday bound and must not match",
		},
		{
			name:   "window in UTC keeps both",
			since:  "2026-10-20T08:00:00+00:00",
			before: "2026-10-20T22:00:00+00:00",
			want:   []string{"doc evening", "doc morning"},
			why:    "both chunks lie inside the window",
		},
		{
			name:   "bound spelled as bare date still works",
			since:  "2026-10-20",
			before: "2026-10-21",
			want:   []string{"doc evening", "doc morning"},
			why:    "the documented bare-date form must keep its meaning",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.SearchDocChunksFTS("doc", "los56e-window", "", tc.since, tc.before, 10, nil)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			seen := map[string]bool{}
			for _, r := range got {
				seen[r.Content] = true
			}
			for _, want := range tc.want {
				if !seen[want] {
					t.Errorf("missing %q (%s); got %v", want, tc.why, los56eContents(got))
				}
				delete(seen, want)
			}
			if len(seen) > 0 {
				t.Errorf("unexpected chunks %v (%s)", los56eKeys(seen), tc.why)
			}
		})
	}
}

// TestLos56eDocsSearchWindowAcceptsOffsetWriterSpelling pins the other stored
// spelling: a chunk stamped in RFC3339 with an offset (a writer inside a
// non-UTC process) must be placed by its instant, not by its 'T'.
func TestLos56eDocsSearchWindowAcceptsOffsetWriterSpelling(t *testing.T) {
	s := mustOpen(t)
	mustUpsertDocSource(t, s, &DocSource{Name: "los56e-offset", Project: ""})
	ds, err := s.GetDocSource("los56e-offset", "")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}
	// 23:30 at +02:00 is 21:30 UTC on the same day.
	los56eDocChunk(t, s, ds.ID, "late", "2026-10-20T23:30:00+02:00")

	got, err := s.SearchDocChunksFTS("late", "los56e-offset", "", "", "2026-10-21T00:00:00+00:00", 10, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("chunk must lie inside the window, got %v", los56eContents(got))
	}

	got, err = s.SearchDocChunksFTS("late", "los56e-offset", "", "", "2026-10-20T20:00:00+00:00", 10, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("21:30 UTC is after the 20:00 bound, got %v", los56eContents(got))
	}
}

func los56eContents(rows []DocChunkResult) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Content)
	}
	return out
}

func los56eKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestLos56eFilterTimeKeepsOffsetInstants guards the helper the fix builds on:
// a bound with an offset must keep its instant, not its spelling.
func TestLos56eFilterTimeKeepsOffsetInstants(t *testing.T) {
	got := ParseFilterTime("2026-10-20T00:00:00+02:00")
	if got.IsZero() || !got.Equal(time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("offset bound must keep its instant, got %v", got)
	}
	if norm := SQLiteFilterTime("2026-10-20T00:00:00+02:00"); norm != "2026-10-19 22:00:00" {
		t.Fatalf("normalized bound = %q, want %q", norm, "2026-10-19 22:00:00")
	}
}

// TestLos56eDocsSemanticSearchHonoursTheWindow covers the review finding on
// YM-308: docs_search documents since/before as result filters, but only the
// BM25 lane applied them. As soon as an embedding provider is configured, the
// merged result carried chunks outside the requested window.
func TestLos56eDocsSemanticSearchHonoursTheWindow(t *testing.T) {
	s := mustOpen(t)
	mustUpsertDocSource(t, s, &DocSource{Name: "los56e-semantic", Project: ""})
	ds, err := s.GetDocSource("los56e-semantic", "")
	if err != nil {
		t.Fatalf("get doc source: %v", err)
	}

	oldID := los56eDocChunk(t, s, ds.ID, "doc old window", "2026-10-01 10:00:00")
	newID := los56eDocChunk(t, s, ds.ID, "doc new window", "2026-10-20 10:00:00")
	vec := []float32{1, 0, 0, 0}
	for _, id := range []int64{oldID, newID} {
		if err := s.SetDocChunkEmbedding(id, vec, "hash"); err != nil {
			t.Fatalf("embed chunk %d: %v", id, err)
		}
	}

	all, err := s.SearchDocChunksSemantic(vec, 10, "los56e-semantic", nil, "", "")
	if err != nil {
		t.Fatalf("search without window: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("without a window both chunks must match, got %v", los56eContents(all))
	}

	inside, err := s.SearchDocChunksSemantic(vec, 10, "los56e-semantic", nil, "2026-10-15", "")
	if err != nil {
		t.Fatalf("search with since: %v", err)
	}
	if len(inside) != 1 || inside[0].Content != "doc new window" {
		t.Fatalf("since bound must drop the older chunk, got %v", los56eContents(inside))
	}

	before, err := s.SearchDocChunksSemantic(vec, 10, "los56e-semantic", nil, "", "2026-10-15T00:00:00+00:00")
	if err != nil {
		t.Fatalf("search with before: %v", err)
	}
	if len(before) != 1 || before[0].Content != "doc old window" {
		t.Fatalf("before bound must drop the newer chunk, got %v", los56eContents(before))
	}

	if _, err := s.SearchDocChunksSemantic(vec, 10, "", nil, "not-a-date", ""); err == nil {
		t.Fatal("an unparsable bound must be reported on this lane too")
	}
}

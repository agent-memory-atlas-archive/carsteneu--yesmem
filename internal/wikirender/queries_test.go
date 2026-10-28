package wikirender

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

func mustOpen(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedLearning(t *testing.T, s *storage.Store, table string, cols []string, args ...any) {
	t.Helper()
	placeholders := make([]string, len(args))
	for i := range args {
		placeholders[i] = "?"
	}
	q := "INSERT INTO " + table + " (" + strings.Join(cols, ",") + ") VALUES (" + strings.Join(placeholders, ",") + ")"
	if _, err := s.DB().Exec(q, args...); err != nil {
		t.Fatalf("seed %s %v: %v", table, args[0], err)
	}
}

// More sessions than the limit share one started_at: the truncation must keep a
// deterministic set. Without a tie-breaker the kept 200 follow whatever row order
// the query plan happens to produce, so the session data a render sees is not
// reproducible (YM-341).
func TestLoadSessionsForIDsTruncatesDeterministically(t *testing.T) {
	s := mustOpen(t)
	const total = 205
	sids := make(map[string]bool, total)
	for i := 1; i <= total; i++ {
		id := fmt.Sprintf("s%04d", i)
		sids[id] = true
		seedLearning(t, s, "sessions",
			[]string{"id", "project", "project_short", "started_at", "jsonl_path", "indexed_at"},
			id, "test", "test", "2026-05-01T10:00:00Z", "/tmp/session.jsonl", "2026-05-01T10:00:00Z")
	}

	rs := newRenderState(&RenderConfig{Project: "test", Store: s})
	if err := rs.loadSessionsForIDs(context.Background(), sids); err != nil {
		t.Fatal(err)
	}
	if len(rs.sessions) != 200 {
		t.Fatalf("loaded %d sessions, want the limit of 200", len(rs.sessions))
	}
	if _, ok := rs.sessions["s0205"]; !ok {
		t.Error("s0205 was dropped: the truncation follows the scan order, not the sort")
	}
	if _, ok := rs.sessions["s0001"]; ok {
		t.Error("s0001 survived the truncation: equal started_at has no deterministic order")
	}
}

func TestLoadLearnings(t *testing.T) {
	s := mustOpen(t)
	seedLearning(t, s, "learnings",
		[]string{"id", "project", "content", "category", "source", "model_used", "created_at"},
		int64(1), "test", "content one", "decision", "user_stated", "opus", "2026-05-01T10:00:00Z")
	seedLearning(t, s, "learnings",
		[]string{"id", "project", "content", "category", "source", "model_used", "created_at"},
		int64(2), "test", "content two", "gotcha", "agreed_upon", "opus", "2026-05-02T10:00:00Z")
	seedLearning(t, s, "learnings",
		[]string{"id", "project", "content", "category", "source", "model_used", "created_at"},
		int64(3), "other", "other project", "decision", "user_stated", "opus", "2026-05-03T10:00:00Z")

	rs := newRenderState(&RenderConfig{Project: "test", Store: s})
	if err := rs.loadLearnings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rs.learnings) != 2 {
		t.Fatalf("expected 2 learnings for test, got %d", len(rs.learnings))
	}
	if rs.learnings[1].Content != "content one" {
		t.Errorf("content mismatch: %q", rs.learnings[1].Content)
	}
}

func TestLoadEntities(t *testing.T) {
	s := mustOpen(t)
	seedLearning(t, s, "learnings",
		[]string{"id", "project", "content", "category", "source", "model_used", "created_at"},
		int64(1), "test", "x", "decision", "user_stated", "opus", "2026-05-01T10:00:00Z")
	seedLearning(t, s, "learnings",
		[]string{"id", "project", "content", "category", "source", "model_used", "created_at"},
		int64(2), "test", "y", "gotcha", "agreed_upon", "opus", "2026-05-02T10:00:00Z")
	seedLearning(t, s, "learning_entities",
		[]string{"learning_id", "value"},
		int64(1), "foo")
	seedLearning(t, s, "learning_entities",
		[]string{"learning_id", "value"},
		int64(1), "bar")
	seedLearning(t, s, "learning_entities",
		[]string{"learning_id", "value"},
		int64(2), "bar")

	rs := newRenderState(&RenderConfig{Project: "test", Store: s})
	rs.loadLearnings(context.Background())
	rs.loadEntities(context.Background())

	if len(rs.entities["foo"]) != 1 || rs.entities["foo"][0] != 1 {
		t.Errorf("foo entities: %v", rs.entities["foo"])
	}
	if len(rs.entities["bar"]) != 2 {
		t.Errorf("bar entities: expected 2, got %d", len(rs.entities["bar"]))
	}
	if len(rs.learnings[1].Entities) != 2 {
		t.Errorf("learning 1 entities: expected 2, got %v", rs.learnings[1].Entities)
	}
}

func TestLoadFileCoverage(t *testing.T) {
	s := mustOpen(t)
	seedLearning(t, s, "file_coverage",
		[]string{"project", "file_path", "directory", "session_count", "last_touched", "operation_types"},
		"test", "internal/x.go", "internal", 5, "2026-05-01T10:00:00Z", "read,grep")
	seedLearning(t, s, "file_coverage",
		[]string{"project", "file_path", "directory", "session_count", "last_touched", "operation_types"},
		"test", "main.go", "", 3, "2026-04-01T10:00:00Z", "edit")

	rs := newRenderState(&RenderConfig{Project: "test", Store: s})
	if err := rs.loadFileCoverage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rs.files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(rs.files))
	}
	if rs.files["internal/x.go"].SessionCount != 5 {
		t.Errorf("session_count: %d", rs.files["internal/x.go"].SessionCount)
	}
}

func TestComputeCoOccurrence(t *testing.T) {
	rs := &renderState{
		byLearning: map[int64][]string{
			1: {"foo", "bar", "baz"},
			2: {"foo", "bar"},
			3: {"foo"},
		},
		cooc: map[string][]CoTopic{},
	}
	rs.computeCoOccurrence()
	if got := rs.cooc["foo"]; len(got) != 1 || got[0].Name != "bar" || got[0].Shared != 2 {
		t.Errorf("expected foo→bar:2, got %+v", got)
	}
	if got := rs.cooc["baz"]; got != nil {
		t.Errorf("baz should have no co-occurrence (shared=1), got %+v", got)
	}
}

func TestIsPathLikeEntity(t *testing.T) {
	cases := map[string]bool{
		"internal/proxy/proxy.go": true,
		"main.go":                 true,
		"README.md":               true,
		"cap-store":               false,
		"telegram":                false,
		"foo.zzz":                 false,
	}
	for in, want := range cases {
		if got := isPathLikeEntity(in); got != want {
			t.Errorf("%q: want %v, got %v", in, want, got)
		}
	}
}

func TestComputeRelatedLearnings(t *testing.T) {
	rs := &renderState{
		learnings: map[int64]Learning{
			1: {ID: 1, Entities: []string{"foo", "bar"}, Content: "L1"},
			2: {ID: 2, Entities: []string{"foo", "baz"}, Content: "L2"},
			3: {ID: 3, Entities: []string{"bar", "qux"}, Content: "L3"},
		},
		entities: map[string][]int64{"foo": {1, 2}, "bar": {1, 3}, "baz": {2}, "qux": {3}},
		related:  map[int64][]RelatedLearning{},
	}
	rs.computeRelatedLearnings()
	if got := len(rs.related[1]); got != 2 {
		t.Errorf("expected 2 related for lid 1, got %d", got)
	}
}

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/wikidir"
)

func newWikiTickTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertLearningForProject(t *testing.T, s *storage.Store, project string) {
	t.Helper()
	_, err := s.DB().Exec(
		`INSERT INTO learnings (session_id, project, category, content, confidence, created_at, model_used, source)
		 VALUES (?, ?, 'gotcha', 'x', 1.0, ?, 'test', 'llm_extracted')`,
		"sess-"+project, project, time.Now().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert learning for %s: %v", project, err)
	}
}

func insertSessionForProject(t *testing.T, s *storage.Store, projectShort, startedAt string) {
	t.Helper()
	_, err := s.DB().Exec(
		`INSERT INTO sessions (id, project, project_short, started_at, message_count, jsonl_path, indexed_at)
		 VALUES (?, ?, ?, ?, 1, '', ?)`,
		"sess-"+projectShort+"-"+startedAt, projectShort, projectShort, startedAt, startedAt,
	)
	if err != nil {
		t.Fatalf("insert session for %s @ %s: %v", projectShort, startedAt, err)
	}
}

// insertLos40bLearningForSession seeds one learning that references the given
// session, the only shape the wiki renderer can see: render.go collects its
// session ids from the project's learnings (YM-266).
func insertLos40bLearningForSession(t *testing.T, s *storage.Store, project, sessionID string) {
	t.Helper()
	_, err := s.DB().Exec(
		`INSERT INTO learnings (session_id, project, category, content, confidence, created_at, model_used, source)
		 VALUES (?, ?, 'gotcha', 'x', 1.0, ?, 'test', 'llm_extracted')`,
		sessionID, project, time.Now().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert learning for session %s: %v", sessionID, err)
	}
}

// --- isLiveProjectPath ---

func TestIsLiveProjectPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"tmp_prefix", "/tmp/foo", false},
		{"tmp_subdir", "/tmp/opencode-xyz123/foo", false},
		{"missing_absolute", "/this/path/does/not/exist/anywhere", false},
		{"relative_kept", "relative/path", true},
		{"empty_kept", "", true},
		{"root_itself", "/", true}, // exists
	}
	// Add a real-existing absolute path (this worktree).
	wd, _ := os.Getwd()
	cases = append(cases, struct {
		name string
		path string
		want bool
	}{"real_worktree", wd, true})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLiveProjectPath(tc.path); got != tc.want {
				t.Errorf("isLiveProjectPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// --- activeProjects dead-path filtering ---

func TestActiveProjects_FiltersTmpAndMissing(t *testing.T) {
	s := newWikiTickTestStore(t)
	// t.TempDir() lives under /tmp on Linux, which the dead-path filter rejects
	// by design. Use the test process CWD as a guaranteed-existing non-/tmp path.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	insertLearningForProject(t, s, "/tmp/opencode-ghost-1")
	insertLearningForProject(t, s, "/tmp/opencode-ghost-2")
	insertLearningForProject(t, s, "/definitely/not/on/disk/path-1")
	insertLearningForProject(t, s, cwd)

	got, err := activeProjects(context.Background(), s)
	if err != nil {
		t.Fatalf("activeProjects: %v", err)
	}

	foundReal := false
	for _, g := range got {
		if g == cwd {
			foundReal = true
			break
		}
	}
	if !foundReal {
		t.Errorf("expected activeProjects to include real path %q, got %v", cwd, got)
	}

	wantExcluded := []string{"/tmp/opencode-ghost-1", "/tmp/opencode-ghost-2", "/definitely/not/on/disk/path-1"}
	for _, excluded := range wantExcluded {
		for _, g := range got {
			if g == excluded {
				t.Errorf("expected activeProjects to exclude dead path %q, but it was included", excluded)
			}
		}
	}
}

// --- projectTierHot ---

func TestProjectTierHot_HotWithin24h(t *testing.T) {
	s := newWikiTickTestStore(t)
	project := t.TempDir() // unused for stat, just a key
	recent := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	insertSessionForProject(t, s, project, recent)

	if !projectTierHot(context.Background(), s, project) {
		t.Errorf("expected project with 1h-old session to be hot, got cold")
	}
}

func TestProjectTierHot_ColdAfter24h(t *testing.T) {
	s := newWikiTickTestStore(t)
	project := t.TempDir()
	old := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	insertSessionForProject(t, s, project, old)

	if projectTierHot(context.Background(), s, project) {
		t.Errorf("expected project with 48h-old session to be cold, got hot")
	}
}

func TestProjectTierHot_NoSessionsIsHot(t *testing.T) {
	s := newWikiTickTestStore(t)
	project := t.TempDir()

	if !projectTierHot(context.Background(), s, project) {
		t.Errorf("expected project with no sessions to be treated as hot (fail-open), got cold")
	}
}

// --- coldTierDueForRender ---

func TestColdTierDueForRender_NeverRendered(t *testing.T) {
	outDir := t.TempDir()
	if !coldTierDueForRender(outDir) {
		t.Errorf("expected coldTierDueForRender=true when no snapshot exists")
	}
}

func TestColdTierDueForRender_RecentSnapshot(t *testing.T) {
	outDir := t.TempDir()
	snapPath := filepath.Join(outDir, ".wiki-snapshot.json")
	if err := os.WriteFile(snapPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	recentMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(snapPath, recentMtime, recentMtime); err != nil {
		t.Fatal(err)
	}
	if coldTierDueForRender(outDir) {
		t.Errorf("expected coldTierDueForRender=false (1h-old snapshot too fresh)")
	}
}

func TestColdTierDueForRender_StaleSnapshot(t *testing.T) {
	outDir := t.TempDir()
	snapPath := filepath.Join(outDir, ".wiki-snapshot.json")
	if err := os.WriteFile(snapPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	staleMtime := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(snapPath, staleMtime, staleMtime); err != nil {
		t.Fatal(err)
	}
	if !coldTierDueForRender(outDir) {
		t.Errorf("expected coldTierDueForRender=true (25h-old snapshot is due)")
	}
}

// --- wikiNeedsRender: session change signal (YM-198) ---

func renderTestWikiDir(t *testing.T, project string) string {
	t.Helper()
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "index.md"), []byte("# wiki\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wikidir.WriteMarker(out, project); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWikiNeedsRender_ExistingSessionChange(t *testing.T) {
	s := newWikiTickTestStore(t)
	project := "proj"
	// The session has to own a learning, otherwise the renderer never shows it
	// and its changes are not part of the signal (YM-266).
	const sessID = "sess-proj-2026-10-01T10:00:00Z"
	insertSessionRow(t, s, sessID, project, "2026-10-01T10:00:00Z")
	insertLos40bLearningForSession(t, s, project, sessID)
	out := renderTestWikiDir(t, project)

	ctx := context.Background()
	writeWikiSnapshot(ctx, s, project, out)
	if wikiNeedsRender(ctx, s, project, out) {
		t.Fatalf("expected no render right after writing the snapshot")
	}

	// The session grows after the session is finished: message_count and
	// ended_at move, started_at stays. The wiki renders both fields.
	if _, err := s.DB().Exec(
		`UPDATE sessions SET message_count = 42, ended_at = '2026-10-01T12:00:00Z' WHERE project_short = ?`, project,
	); err != nil {
		t.Fatal(err)
	}
	if !wikiNeedsRender(ctx, s, project, out) {
		t.Error("expected render after an existing session changed (message_count/ended_at)")
	}
}

func TestWikiNeedsRender_StableAfterSessionChange(t *testing.T) {
	s := newWikiTickTestStore(t)
	project := "proj"
	const sessID = "sess-proj-2026-10-01T10:00:00Z"
	insertSessionRow(t, s, sessID, project, "2026-10-01T10:00:00Z")
	insertLos40bLearningForSession(t, s, project, sessID)
	out := renderTestWikiDir(t, project)

	ctx := context.Background()
	writeWikiSnapshot(ctx, s, project, out)
	if _, err := s.DB().Exec(`UPDATE sessions SET message_count = 42 WHERE project_short = ?`, project); err != nil {
		t.Fatal(err)
	}
	writeWikiSnapshot(ctx, s, project, out)
	if wikiNeedsRender(ctx, s, project, out) {
		t.Error("snapshot written after the change must be stable")
	}
}

// A session that owns no learning is on no wiki page, so its counters must not
// trigger a render. The signal used to aggregate every session of the project
// (a superset of the rendered set), which re-rendered every hot project on
// every tick (YM-266 / DEV-DT43).
func TestWikiNeedsRender_IgnoresSessionsWithoutLearnings(t *testing.T) {
	s := newWikiTickTestStore(t)
	project := "proj"
	// Two real sessions: one grows without ever owning a learning, the other owns
	// one and keeps the project active. Only the second one is rendered.
	const unlinked = "sess-unlinked"
	const rendered = "sess-rendered"
	insertSessionRow(t, s, unlinked, project, "2026-10-01T10:00:00Z")
	insertSessionRow(t, s, rendered, project, "2026-10-02T10:00:00Z")
	insertLos40bLearningForSession(t, s, project, rendered)
	out := renderTestWikiDir(t, project)

	ctx := context.Background()
	writeWikiSnapshot(ctx, s, project, out)
	if _, err := s.DB().Exec(`UPDATE sessions SET message_count = 42 WHERE id = ?`, unlinked); err != nil {
		t.Fatal(err)
	}
	if wikiNeedsRender(ctx, s, project, out) {
		t.Error("a session without learnings is not rendered, its change must not force a render")
	}
}

// The signal must not be capped at the session page's 200-row window: the file
// pages list start times and message counts of the project's learning-linked
// sessions beyond it (wikirender.loadFileSessions, top 10 per path). A change to
// the oldest session is therefore still rendered content and has to move the
// signal (YM-266 review finding).
func TestWikiNeedsRender_DetectsSessionBeyondThePageWindow(t *testing.T) {
	s := newWikiTickTestStore(t)
	project := "proj"
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	var oldest string
	for i := 0; i < 201; i++ {
		started := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		id := "sess-" + started
		insertSessionRow(t, s, id, project, started)
		insertLos40bLearningForSession(t, s, project, id)
		if i == 0 {
			oldest = id
		}
	}
	out := renderTestWikiDir(t, project)

	ctx := context.Background()
	writeWikiSnapshot(ctx, s, project, out)
	if _, err := s.DB().Exec(`UPDATE sessions SET message_count = 42 WHERE id = ?`, oldest); err != nil {
		t.Fatal(err)
	}
	if !wikiNeedsRender(ctx, s, project, out) {
		t.Error("session number 201 is rendered on the file pages, its change must force a render")
	}
}

// The file list renders session_count and last_touched from file_coverage, which
// the indexer fills from every indexed session. That content has no learning
// behind it, so the snapshot needs its own component for it (YM-266).
func TestWikiNeedsRender_DetectsFileCoverageChange(t *testing.T) {
	s := newWikiTickTestStore(t)
	project := "proj"
	insertLearningForProject(t, s, project)
	if _, err := s.DB().Exec(
		`INSERT INTO file_coverage (project, file_path, directory, session_count, last_touched)
		 VALUES (?, 'internal/x.go', 'internal', 3, '2026-10-01T10:00:00Z')`, project,
	); err != nil {
		t.Fatal(err)
	}
	out := renderTestWikiDir(t, project)

	ctx := context.Background()
	writeWikiSnapshot(ctx, s, project, out)
	if _, err := s.DB().Exec(
		`UPDATE file_coverage SET session_count = 9, last_touched = '2026-10-02T10:00:00Z' WHERE project = ?`, project,
	); err != nil {
		t.Fatal(err)
	}
	if !wikiNeedsRender(ctx, s, project, out) {
		t.Error("the file list renders file_coverage, its change must force a render")
	}
}

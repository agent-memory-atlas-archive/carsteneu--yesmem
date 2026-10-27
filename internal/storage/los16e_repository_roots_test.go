package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// los16eInsertSession records a session whose project id is its absolute path,
// the shape the indexer writes since the path rekeying.
func los16eInsertSession(t *testing.T, s *Store, id, project string, at time.Time) {
	t.Helper()
	if err := s.UpsertSession(&models.Session{
		ID: id, Project: project, ProjectShort: project, MessageCount: 3,
		StartedAt: at, JSONLPath: "/" + id + ".jsonl", IndexedAt: at,
	}); err != nil {
		t.Fatalf("upsert session %s: %v", id, err)
	}
}

// TestListProjectsWithRepositoryRootsIncludesWorktreeParentLos16e (YM-382 [BRF-B15]):
// ListProjects counts only projects that carry their own sessions, so a repository
// whose work all happened inside worktree checkouts never appears in the list — and
// the briefing refresh loop, which keys its worklist on that list, never generates a
// briefing for the parent. The parent must be added, and exactly once.
func TestListProjectsWithRepositoryRootsIncludesWorktreeParentLos16e(t *testing.T) {
	s := mustOpen(t)

	root := filepath.Join(t.TempDir(), "los16e-repo")
	worktree := filepath.Join(root, ".worktrees", "feat")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	secondWorktree := filepath.Join(root, ".worktrees", "fix")
	if err := os.MkdirAll(secondWorktree, 0o755); err != nil {
		t.Fatalf("mkdir second worktree: %v", err)
	}
	los16eInsertSession(t, s, "sess-los16e-wt", worktree, time.Now().Add(-time.Hour))
	// Two copies of the same repository must still yield one root row.
	los16eInsertSession(t, s, "sess-los16e-wt2", secondWorktree, time.Now().Add(-2*time.Hour))

	projects, err := s.ListProjectsWithRepositoryRoots()
	if err != nil {
		t.Fatalf("list projects with roots: %v", err)
	}

	var rootRows, worktreeRows int
	for _, p := range projects {
		switch p.ProjectShort {
		case root:
			rootRows++
		case worktree, secondWorktree:
			worktreeRows++
		default:
			t.Errorf("unexpected project row: short=%q project=%q", p.ProjectShort, p.Project)
		}
	}
	if worktreeRows != 2 {
		t.Errorf("each worktree project must stay in the list once, got %d", worktreeRows)
	}
	if rootRows != 1 {
		t.Fatalf("the repository root of a worktree-only project must be listed once, got %d (rows=%v)", rootRows, projects)
	}
	for _, p := range projects {
		if p.ProjectShort != root {
			continue
		}
		if p.Project != root {
			t.Errorf("root row must carry the path in both fields, got project=%q", p.Project)
		}
		// All the work lives in the worktrees, so the root itself owns no session.
		if p.SessionCount != 0 {
			t.Errorf("root must not borrow the worktree's session count, got %d", p.SessionCount)
		}
		if p.LastActive == "" {
			t.Error("root row must carry the repository's latest activity, not an empty timestamp")
		}
	}
}

// TestListProjectsWithRepositoryRootsSkipsClimbingRootLos16e (YM-382, Review-Runde
// 6): a session opened at the root resolves through OwningProject to the nearest
// ancestor that carries sessions, so a briefing cached under the root key would
// never be read. Such a root gets no row.
func TestListProjectsWithRepositoryRootsSkipsClimbingRootLos16e(t *testing.T) {
	s := mustOpen(t)

	parent := filepath.Join(t.TempDir(), "collection")
	root := filepath.Join(parent, "los16e-climb")
	worktree := filepath.Join(root, ".worktrees", "feat")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	los16eInsertSession(t, s, "sess-los16e-climb-wt", worktree, time.Now())
	// The collection directory carries a session of its own, so the read path climbs.
	los16eInsertSession(t, s, "sess-los16e-climb-parent", parent, time.Now())
	if s.OwningProject(root) == root {
		t.Fatalf("precondition: the read path must climb for this fixture")
	}

	projects, err := s.ListProjectsWithRepositoryRoots()
	if err != nil {
		t.Fatalf("list projects with roots: %v", err)
	}
	for _, p := range projects {
		if p.ProjectShort == root {
			t.Fatalf("a root the read path never resolves to must not gain a row")
		}
	}
}

// TestListProjectsWithRepositoryRootsKeepsOwnedRootLos16e: a repository that has
// its own sessions keeps its single row with its own count, worktrees or not, and a
// project outside any worktree gains no synthetic row.
func TestListProjectsWithRepositoryRootsKeepsOwnedRootLos16e(t *testing.T) {
	s := mustOpen(t)

	root := filepath.Join(t.TempDir(), "los16e-owned")
	worktree := filepath.Join(root, ".worktrees", "feat")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	now := time.Now()
	los16eInsertSession(t, s, "sess-los16e-owned-1", root, now.Add(-2*time.Hour))
	los16eInsertSession(t, s, "sess-los16e-owned-2", root, now.Add(-time.Hour))
	los16eInsertSession(t, s, "sess-los16e-owned-wt", worktree, now)

	plain := filepath.Join(t.TempDir(), "los16e-plain")
	los16eInsertSession(t, s, "sess-los16e-plain", plain, now)

	projects, err := s.ListProjectsWithRepositoryRoots()
	if err != nil {
		t.Fatalf("list projects with roots: %v", err)
	}

	counts := make(map[string]int)
	rows := make(map[string]int)
	for _, p := range projects {
		rows[p.ProjectShort]++
		counts[p.ProjectShort] = p.SessionCount
	}
	if rows[root] != 1 {
		t.Errorf("an owned root must keep exactly one row, got %d", rows[root])
	}
	if counts[root] != 2 {
		t.Errorf("an owned root must keep its own session count, got %d", counts[root])
	}
	if rows[plain] != 1 || counts[plain] != 1 {
		t.Errorf("a project without worktrees must be listed unchanged, rows=%d count=%d", rows[plain], counts[plain])
	}
	if rows[worktree] != 1 {
		t.Errorf("the worktree project must stay listed, got %d rows", rows[worktree])
	}
}

// TestListProjectsWithRepositoryRootsSkipsPlainSubdirectoryLos16e (YM-382,
// Merge-Block, Zusammenspiel mit los-55d): once RepositoryRoot resolves an
// enclosing checkout, a session in a plain subdirectory maps to its repository root.
// Without the worktree-marker gate that would plant a root row, and its briefing
// would be assembled from the canonical basename instead of the rows below it.
func TestListProjectsWithRepositoryRootsSkipsPlainSubdirectoryLos16e(t *testing.T) {
	s := mustOpen(t)

	repo := filepath.Join(t.TempDir(), "los16e-sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sub := filepath.Join(repo, "internal", "daemon")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	los16eInsertSession(t, s, "sess-los16e-sub", sub, time.Now())

	projects, err := s.ListProjectsWithRepositoryRoots()
	if err != nil {
		t.Fatalf("list projects with roots: %v", err)
	}
	if len(projects) != 1 || projects[0].ProjectShort != sub {
		t.Fatalf("a plain subdirectory must not gain a root row, got %d rows: %v", len(projects), projects)
	}
}

// TestResolveCanonicalProjectWorktreeOnlyRootLos16e (YM-382 [BRF-B15]):
// ListProjects counts only projects that carry their own sessions, so a repository
// whose work happened entirely inside worktree checkouts never got a briefing. The
// project keeps its own path as the scope, and the read reaches the rows its
// worktrees wrote through the subtree predicate.
func TestResolveCanonicalProjectWorktreeOnlyRootLos16e(t *testing.T) {
	s := mustOpen(t)

	root := filepath.Join(t.TempDir(), "los16e-canon")
	worktree := filepath.Join(root, ".worktrees", "feat")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	los16eInsertSession(t, s, "sess-los16e-canon-wt", worktree, time.Now())
	if _, err := s.InsertLearning(&models.Learning{
		Content: "PROBE_LEARNING_CONTENT", Category: "gotcha", Source: "user_stated",
		SessionID: "sess-los16e-canon-wt", Project: worktree, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}

	if got := s.resolveCanonicalProject(root); got != root {
		t.Errorf("a root without rows of its own must keep its path scope, got %q", got)
	}
	learnings, err := s.ActiveLearningsForProject(root, "", 0)
	if err != nil {
		t.Fatalf("get active learnings for root: %v", err)
	}
	if len(learnings) != 1 {
		t.Fatalf("the root must see the learning its worktree wrote, got %d", len(learnings))
	}
	if learnings[0].Content != "PROBE_LEARNING_CONTENT" {
		t.Errorf("unexpected learning content %q", learnings[0].Content)
	}
}

// TestResolveCanonicalProjectKeepsPathScopeWithoutWorktreeRowsLos16e (YM-382,
// Kaltrreview-Runde 2, Befund 1.2): the basename is only the right scope when the
// path really is the root of existing worktree rows. Without worktree rows it
// widens the read to every repository whose folder has the same name.
func TestResolveCanonicalProjectKeepsPathScopeWithoutWorktreeRowsLos16e(t *testing.T) {
	s := mustOpen(t)

	first := filepath.Join(t.TempDir(), "001", "yesmem")
	second := filepath.Join(t.TempDir(), "002", "yesmem")
	los16eInsertSession(t, s, "sess-los16e-same-1", first, time.Now())
	if _, err := s.InsertLearning(&models.Learning{
		Content: "REAL_PROJECT_SECRET", Category: "gotcha", Source: "user_stated",
		SessionID: "sess-los16e-same-1", Project: first, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}

	if got := s.resolveCanonicalProject(second); got != second {
		t.Errorf("a path without worktree rows must keep its own scope, got %q", got)
	}
	learnings, err := s.ActiveLearningsForProject(second, "", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}
	if len(learnings) != 0 {
		t.Fatalf("a same-named repository must not see foreign learnings, got %d", len(learnings))
	}
}

// TestResolveCanonicalProjectKeepsPathValuedRowLos16e (YM-382, Kaltrreview-Runde 2,
// Befund 1.1): a row whose canonical_project carries the path itself resolves
// through the DB lookup, and the resolve path that closes an open task matches on
// that value. Overriding it with the basename makes such a row unreachable.
func TestResolveCanonicalProjectKeepsPathValuedRowLos16e(t *testing.T) {
	s := mustOpen(t)

	project := filepath.Join(t.TempDir(), "los16e-pathvalued")
	if _, err := s.InsertLearning(&models.Learning{
		Content: "open task", Category: "unfinished", Source: "user_stated", TaskType: "task",
		SessionID: "sess-los16e-pathvalued", Project: project, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE learnings SET canonical_project = ? WHERE project = ?`, project, project); err != nil {
		t.Fatalf("set canonical project: %v", err)
	}

	if got := s.resolveCanonicalProject(project); got != project {
		t.Errorf("a row carrying the path as canonical_project must stay reachable, got %q", got)
	}
	learnings, err := s.ActiveLearningsForProject(project, "unfinished", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}
	if len(learnings) != 1 {
		t.Fatalf("the open task must stay reachable from its own path, got %d", len(learnings))
	}
}

// TestResolveCanonicalProjectKeepsPathValuedRowWithWorktreeRowsLos16e (YM-382,
// Kaltrreview-Runde 3, F1): the two cases meet — a path-valued row of the root and
// worktree rows below it. The fallback must not hide the root's own row; it exists
// for roots that have none.
func TestResolveCanonicalProjectKeepsPathValuedRowWithWorktreeRowsLos16e(t *testing.T) {
	s := mustOpen(t)

	root := filepath.Join(t.TempDir(), "los16e-mixed")
	worktree := filepath.Join(root, ".worktrees", "feat")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	los16eInsertSession(t, s, "sess-los16e-mixed-root", root, time.Now())
	if _, err := s.InsertLearning(&models.Learning{
		Content: "open task at the root", Category: "unfinished", Source: "user_stated", TaskType: "task",
		SessionID: "sess-los16e-mixed-root", Project: root, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert root learning: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE learnings SET canonical_project = ? WHERE project = ?`, root, root); err != nil {
		t.Fatalf("set canonical project: %v", err)
	}
	los16eInsertSession(t, s, "sess-los16e-mixed-wt", worktree, time.Now())
	if _, err := s.InsertLearning(&models.Learning{
		Content: "worktree gotcha", Category: "gotcha", Source: "user_stated",
		SessionID: "sess-los16e-mixed-wt", Project: worktree, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert worktree learning: %v", err)
	}

	if got := s.resolveCanonicalProject(root); got != root {
		t.Errorf("the root's own path-valued row must stay authoritative, got %q", got)
	}
	learnings, err := s.ActiveLearningsForProject(root, "unfinished", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}
	if len(learnings) != 1 {
		t.Fatalf("the root's open task must stay reachable, got %d", len(learnings))
	}
}

// TestLearningsReadKeepsCollectionDirectoryNarrowLos16e (YM-382, Merge-Block Runde
// 5, Review-Befund F1): a directory that merely CONTAINS other projects is not a
// project. Folding the subtree there made /home/tester/memory answer with the rows of
// every repository below it (measured 3.609 to 14.255 rows on the live DB). The
// subtree is only folded for a repository checkout or a worktree copy.
func TestLearningsReadKeepsCollectionDirectoryNarrowLos16e(t *testing.T) {
	s := mustOpen(t)

	home := filepath.Join(t.TempDir(), "home")
	first := filepath.Join(home, "repoA")
	second := filepath.Join(home, "repoB")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, tc := range []struct{ id, project, content string }{
		{"sess-los16e-home-a", first, "OWN_A"},
		{"sess-los16e-home-b", second, "OTHER_B"},
	} {
		los16eInsertSession(t, s, tc.id, tc.project, time.Now())
		if _, err := s.InsertLearning(&models.Learning{
			Content: tc.content, Category: "gotcha", Source: "user_stated",
			SessionID: tc.id, Project: tc.project, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("insert learning: %v", err)
		}
	}

	learnings, err := s.ActiveLearningsForProject(home, "", 0)
	if err != nil {
		t.Fatalf("get active learnings for the collection directory: %v", err)
	}
	if len(learnings) != 0 {
		for _, l := range learnings {
			t.Errorf("a collection directory must not answer with %q", l.Content)
		}
		t.Fatalf("a collection directory must stay narrow, got %d rows", len(learnings))
	}

	own, err := s.ActiveLearningsForProject(first, "", 0)
	if err != nil {
		t.Fatalf("get active learnings for the repository: %v", err)
	}
	var sawOwn, sawForeign bool
	for _, l := range own {
		if l.Content == "OWN_A" {
			sawOwn = true
		}
		if l.Content == "OTHER_B" {
			sawForeign = true
		}
	}
	if sawForeign {
		t.Error("a repository must not see its sibling's learning")
	}
	if !sawOwn {
		t.Error("a repository must still see its own learning")
	}
}

// TestLearningsReadScopeServesLegacyShortNameRowsLos16e (YM-382, Merge-Block
// Runde 5, Gegenprobe zum Scope-Praedikat): rows written before the path rekeying
// carry the short name in project. The path scope keeps them reachable through the
// canonical clause, so the predicate narrows without losing history.
func TestLearningsReadScopeServesLegacyShortNameRowsLos16e(t *testing.T) {
	s := mustOpen(t)

	root := filepath.Join(t.TempDir(), "los16e-legacy")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if _, err := s.InsertLearning(&models.Learning{
		Content: "LEGACY_ROW", Category: "gotcha", Source: "user_stated",
		SessionID: "sess-los16e-legacy", Project: "los16e-legacy", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}

	learnings, err := s.ActiveLearningsForProject(root, "", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}
	for _, l := range learnings {
		if l.Content == "LEGACY_ROW" {
			return
		}
	}
	t.Fatalf("a legacy short-name row of this repository must stay reachable, got %d rows", len(learnings))
}

// TestLearningsReadKeepSiblingProjectsOfOneCheckoutApartLos16e (YM-382, Merge-Block
// Runde 5): two projects that are plain subdirectories of ONE checkout carry their
// own learnings. A read may fold the worktree copies of a project, but it must not
// ascend to the enclosing checkout: that would merge sibling projects with each
// other. The read root therefore comes from the worktree markers alone, never from
// models.RepositoryRoot, which resolves the enclosing checkout since YM-321.
func TestLearningsReadKeepSiblingProjectsOfOneCheckoutApartLos16e(t *testing.T) {
	s := mustOpen(t)

	repo := filepath.Join(t.TempDir(), "los16e-checkout")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	app := filepath.Join(repo, "app")
	ops := filepath.Join(repo, "ops")
	for _, dir := range []string{app, ops} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	los16eInsertSession(t, s, "sess-los16e-app", app, time.Now())
	los16eInsertSession(t, s, "sess-los16e-ops", ops, time.Now())
	for _, p := range []struct{ project, content string }{
		{app, "APP_LEARNING"},
		{ops, "OPS_LEARNING"},
	} {
		if _, err := s.InsertLearning(&models.Learning{
			Content: p.content, Category: "gotcha", Source: "user_stated",
			SessionID: "sess-los16e-" + p.content[:3], Project: p.project, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("insert learning for %s: %v", p.project, err)
		}
	}

	for _, tc := range []struct{ project, own, foreign string }{
		{app, "APP_LEARNING", "OPS_LEARNING"},
		{ops, "OPS_LEARNING", "APP_LEARNING"},
	} {
		learnings, err := s.ActiveLearningsForProject(tc.project, "", 0)
		if err != nil {
			t.Fatalf("get active learnings for %s: %v", tc.project, err)
		}
		var sawOwn, sawForeign bool
		for _, l := range learnings {
			if l.Content == tc.own {
				sawOwn = true
			}
			if l.Content == tc.foreign {
				sawForeign = true
			}
		}
		if sawForeign {
			t.Errorf("%s must not see the sibling project's learning %s", tc.project, tc.foreign)
		}
		if !sawOwn {
			t.Errorf("%s must see its own learning %s", tc.project, tc.own)
		}
	}
}

// TestLearningsReadKeepSameNamedRepositoriesApartLos16e (YM-382, Merge-Block):
// the isolation between two repositories that share a folder name must not depend
// on a decision made before the read. The read scopes an absolute path by its own
// subtree, so a row another repository writes at any time stays invisible here.
func TestLearningsReadKeepSameNamedRepositoriesApartLos16e(t *testing.T) {
	s := mustOpen(t)

	first := filepath.Join(t.TempDir(), "001", "yesmem")
	second := filepath.Join(t.TempDir(), "002", "yesmem")
	for i, root := range []string{first, second} {
		worktree := filepath.Join(root, ".worktrees", "feat")
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatalf("mkdir worktree: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
		sessionID := "sess-los16e-read-" + string(rune('a'+i))
		los16eInsertSession(t, s, sessionID, worktree, time.Now())
		if _, err := s.InsertLearning(&models.Learning{
			Content: "SECRET_OF_" + filepath.Base(filepath.Dir(root)), Category: "gotcha", Source: "user_stated",
			SessionID: sessionID, Project: worktree, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("insert learning: %v", err)
		}
	}

	assertNoForeign := func(root, foreign string) {
		t.Helper()
		learnings, err := s.ActiveLearningsForProject(root, "", 0)
		if err != nil {
			t.Fatalf("get active learnings for %s: %v", root, err)
		}
		for _, l := range learnings {
			if l.Content == foreign {
				t.Fatalf("%s must not see %s", root, foreign)
			}
		}
	}
	assertSees := func(root, own string) {
		t.Helper()
		learnings, err := s.ActiveLearningsForProject(root, "", 0)
		if err != nil {
			t.Fatalf("get active learnings for %s: %v", root, err)
		}
		for _, l := range learnings {
			if l.Content == own {
				return
			}
		}
		t.Fatalf("%s must see the learning its worktree wrote, got %d rows", root, len(learnings))
	}

	// Both repositories work only in worktrees, and each one keeps its own rows.
	assertSees(second, "SECRET_OF_002")
	assertNoForeign(second, "SECRET_OF_001")
	assertNoForeign(first, "SECRET_OF_002")

	// The former race: a foreign row written between the scope decision and the
	// read. There is no decision to race any more, and the row stays invisible.
	late := filepath.Join(first, ".worktrees", "late")
	if err := os.MkdirAll(late, 0o755); err != nil {
		t.Fatalf("mkdir late worktree: %v", err)
	}
	los16eInsertSession(t, s, "sess-los16e-read-late", late, time.Now())
	if _, err := s.InsertLearning(&models.Learning{
		Content: "LATE_OF_001", Category: "gotcha", Source: "user_stated",
		SessionID: "sess-los16e-read-late", Project: late, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert late learning: %v", err)
	}
	assertNoForeign(second, "LATE_OF_001")
	assertSees(first, "LATE_OF_001")
}

// TestResolveCanonicalProjectKeepsSameNamedWorktreeOnlyRootIsolatedLos16e (YM-382,
// Security-Review, Befund 2): two repositories with the same folder name, both
// working only in worktrees, share the canonical basename. A gate that merely asks
// "is there any row below this root" fires for both and lets each read the other's
// learnings; the gate has to require that no foreign root carries the basename too.
func TestResolveCanonicalProjectKeepsSameNamedWorktreeOnlyRootIsolatedLos16e(t *testing.T) {
	s := mustOpen(t)

	first := filepath.Join(t.TempDir(), "001", "yesmem")
	second := filepath.Join(t.TempDir(), "002", "yesmem")
	for i, root := range []string{first, second} {
		worktree := filepath.Join(root, ".worktrees", "feat")
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatalf("mkdir worktree: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
		sessionID := "sess-los16e-iso-" + string(rune('a'+i))
		los16eInsertSession(t, s, sessionID, worktree, time.Now())
		if _, err := s.InsertLearning(&models.Learning{
			Content: "SECRET_OF_" + filepath.Base(filepath.Dir(root)), Category: "gotcha", Source: "user_stated",
			SessionID: sessionID, Project: worktree, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("insert learning: %v", err)
		}
	}

	if got := s.resolveCanonicalProject(second); got != second {
		t.Errorf("a root sharing its basename with another repository must keep its own scope, got %q", got)
	}
	learnings, err := s.ActiveLearningsForProject(second, "", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}
	for _, l := range learnings {
		if l.Content == "SECRET_OF_001" {
			t.Fatalf("the same-named repository must not see foreign learnings, got %q", l.Content)
		}
	}
}

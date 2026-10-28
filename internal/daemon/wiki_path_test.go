package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/wikidir"
)

// scanDouble fails every scan so runWikiOnce renders without a code graph.
// Keeps the wiki-tick tests off the real codebase-memory-mcp binary.
type scanDouble struct{}

func (scanDouble) Scan(string) (*codescan.ScanResult, error) {
	return nil, errors.New("scan disabled in test")
}

func runWikiTickForTest(t *testing.T, s *storage.Store, outRoot string) {
	t.Helper()
	scanner := codescan.NewCachedScanner(scanDouble{})
	runWikiOnce(context.Background(), s, outRoot, scanner)
}

// wikiDirsFor returns all directory names under root whose .project marker
// names project.
func wikiDirsFor(t *testing.T, root, project string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read wiki root %s: %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if wikidir.MarkerProject(filepath.Join(root, e.Name())) == project {
			out = append(out, e.Name())
		}
	}
	return out
}

// worktreeRoot returns the repository worktree root the test runs in. Tests
// execute with CWD = package dir, so the root is two levels up.
func worktreeRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := filepath.Abs(filepath.Join(wd, "..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("no worktree root at %s: %v", root, err)
	}
	return root
}

// fixtureProjectDir creates an existing, non-/tmp project path below the
// worktree. t.TempDir() lives under /tmp, which activeProjects rejects by
// design, so these fixtures cannot use it.
func fixtureProjectDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(worktreeRoot(t), ".yesmem", "tmp", "wikipath", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir fixture %s: %v", dir, err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Join(worktreeRoot(t), ".yesmem", "tmp", "wikipath")) })
	return dir
}

// sameNameFixtures returns two existing project paths that share a basename.
func sameNameFixtures(t *testing.T) (string, string) {
	t.Helper()
	a := fixtureProjectDir(t, "coll-a/samename")
	b := fixtureProjectDir(t, "coll-b/samename")
	return a, b
}

// --- YM-6: the writer must use the same wiki directory as the readers ---

// Regression YM-6: the writer used the full project path, so the wiki landed in
// wiki/home/tester/memory/yesmem/ while every reader (code map, wiki-first
// directive, docs) points at wiki/yesmem/.
func TestRunWikiOnce_WritesToProjectShortNameDir(t *testing.T) {
	s := mustStore(t)
	projDir := fixtureProjectDir(t, "single/yesmemtest")
	insertLearningForProject(t, s, projDir)
	insertSessionForProject(t, s, projDir, time.Now().Format(time.RFC3339))

	outRoot := t.TempDir()
	runWikiTickForTest(t, s, outRoot)

	want := filepath.Join(outRoot, filepath.Base(projDir), "index.md")
	if _, err := os.Stat(want); err != nil {
		got, _ := os.ReadDir(outRoot)
		names := make([]string, 0, len(got))
		for _, e := range got {
			names = append(names, e.Name())
		}
		t.Fatalf("expected wiki at %s (stat err=%v); wiki root holds %v", want, err, names)
	}
}

// Regression YM-6: two live projects sharing a basename must not write into
// the same directory. Each project gets its own directory and a .project
// marker recording the owner.
func TestRunWikiOnce_SameNameProjectsDoNotCollide(t *testing.T) {
	s := mustStore(t)
	a, b := sameNameFixtures(t)
	for _, p := range []string{a, b} {
		insertLearningForProject(t, s, p)
		insertSessionForProject(t, s, p, time.Now().Format(time.RFC3339))
	}

	outRoot := t.TempDir()
	runWikiTickForTest(t, s, outRoot)
	// Second tick: a project must keep the directory it rendered into.
	runWikiTickForTest(t, s, outRoot)

	for _, p := range []string{a, b} {
		dirs := wikiDirsFor(t, outRoot, p)
		if len(dirs) != 1 {
			t.Fatalf("project %s: expected exactly one wiki dir with a .project marker, got %v", p, dirs)
		}
		if _, err := os.Stat(filepath.Join(outRoot, dirs[0], "index.md")); err != nil {
			t.Errorf("project %s: no index.md in %s: %v", p, dirs[0], err)
		}
	}

	dirsA := wikiDirsFor(t, outRoot, a)
	dirsB := wikiDirsFor(t, outRoot, b)
	if dirsA[0] == dirsB[0] {
		t.Fatalf("both projects mapped to the same wiki dir %q", dirsA[0])
	}
	// The plain basename stays readable for one of them.
	plain := filepath.Base(a)
	if dirsA[0] != plain && dirsB[0] != plain {
		t.Errorf("expected one project to own the plain dir %q, got %q and %q", plain, dirsA[0], dirsB[0])
	}
}

// Regression YM-6: a project identifier without a usable directory name must
// not make the renderer write into the wiki root itself.
func TestRunWikiOnce_SkipsUnusableProjectNames(t *testing.T) {
	s := mustStore(t)
	insertLearningForProject(t, s, "/")
	insertSessionForProject(t, s, "/", time.Now().Format(time.RFC3339))

	outRoot := t.TempDir()
	runWikiTickForTest(t, s, outRoot)

	if _, err := os.Stat(filepath.Join(outRoot, "index.md")); !os.IsNotExist(err) {
		t.Fatalf("wiki root was rendered into directly (err=%v)", err)
	}
}

// --- YM-6: snapshot must notice edits of existing learnings ---

// Regression YM-6: the snapshot compared COUNT and MAX(created_at) only, so an
// in-place edit of an existing learning never triggered a re-render.
func TestWikiNeedsRender_ReactsToContentEdit(t *testing.T) {
	s := mustStore(t)
	project := "wikineedsrender-project"
	insertLearningForProject(t, s, project)

	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "index.md"), []byte("# wiki\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wikidir.WriteMarker(outDir, project); err != nil {
		t.Fatal(err)
	}
	writeWikiSnapshot(context.Background(), s, project, outDir)

	ctx := context.Background()
	if wikiNeedsRender(ctx, s, project, outDir) {
		t.Fatalf("expected no re-render right after taking the snapshot")
	}

	if _, err := s.DB().Exec(
		`UPDATE learnings SET content = content || ' edited and therefore longer' WHERE project = ?`, project,
	); err != nil {
		t.Fatalf("edit learning: %v", err)
	}

	if !wikiNeedsRender(ctx, s, project, outDir) {
		t.Errorf("content edit of an existing learning did not trigger a re-render")
	}
}

// Regression/cold-review F2b: an unmarked directory (the pre-fix layout) is not
// the project's own and must be re-rendered, not trusted.
func TestWikiNeedsRender_UnmarkedDirIsRendered(t *testing.T) {
	s := mustStore(t)
	project := "wikineedsrender-unmarked"
	insertLearningForProject(t, s, project)

	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "index.md"), []byte("# wiki\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeWikiSnapshot(context.Background(), s, project, outDir)

	if !wikiNeedsRender(context.Background(), s, project, outDir) {
		t.Errorf("a directory without this project's marker must be rendered")
	}
}

// Regression YM-6: a supersede/replace that keeps the active count and the
// newest created_at identical must still trigger a re-render.
func TestWikiSnapshot_DetectsSupersedeWithOlderReplacement(t *testing.T) {
	s := mustStore(t)
	project := "wikisnapshot-project"
	insertLearningForProject(t, s, project)

	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "index.md"), []byte("# wiki\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wikidir.WriteMarker(outDir, project); err != nil {
		t.Fatal(err)
	}
	writeWikiSnapshot(context.Background(), s, project, outDir)

	// A replacement learning created BEFORE the current one, replacing it:
	// count stays 1, MAX(created_at) goes backwards.
	var id int64
	if err := s.DB().QueryRow(`SELECT id FROM learnings WHERE project = ?`, project).Scan(&id); err != nil {
		t.Fatalf("read learning id: %v", err)
	}
	older := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	res, err := s.DB().Exec(
		`INSERT INTO learnings (session_id, project, category, content, confidence, created_at, model_used, source, supersedes)
		 VALUES (?, ?, 'gotcha', 'replacement text', 1.0, ?, 'test', 'llm_extracted', ?)`,
		sessIDFor(project), project, older, id,
	)
	if err != nil {
		t.Fatalf("insert replacement: %v", err)
	}
	newID, _ := res.LastInsertId()
	if _, err := s.DB().Exec(
		`UPDATE learnings SET superseded_by = ? WHERE id = ?`, newID, id,
	); err != nil {
		t.Fatalf("supersede old learning: %v", err)
	}

	if !wikiNeedsRender(context.Background(), s, project, outDir) {
		t.Errorf("supersede with an older replacement did not trigger a re-render")
	}
}

func sessIDFor(project string) string { return fmt.Sprintf("sess-%s", project) }

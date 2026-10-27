package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// YM-442 (Los 61d): a scan rooted below the checkout must not replace the
// repository index. ScanCacheKey folds a subdirectory onto the repository root
// (internal/codescan/cache.go), so a session started in "internal/" persists its
// subdirectory scan under the root's key — and replaceScanFiles deleted every
// indexed path of the whole repository on the way in.
func TestLos61dSubscanDoesNotReplaceRepoIndex(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	root := t.TempDir()
	repoDoc := los61dScanDoc(t, root, "internal/proxy/proxy.go", "internal/storage/store.go")
	if err := s.PersistScan(root, repoDoc, "head1", 0); err != nil {
		t.Fatalf("persist repo scan: %v", err)
	}

	subDoc := los61dScanDoc(t, filepath.Join(root, "internal"), "proxy/proxy.go")
	err := s.PersistScan(root, subDoc, "head1", 0)
	if !errors.Is(err, ErrScanRootBelowCheckout) {
		t.Fatalf("a scan rooted below the checkout must be refused, got err = %v", err)
	}

	if !s.IsFileInCodeIndex(root, "internal/proxy/proxy.go") {
		t.Error("the repository index must keep its paths after a refused subordinate scan")
	}
	if !s.IsFileInCodeIndex(root, "internal/storage/store.go") {
		t.Error("no indexed path of the repository may be lost")
	}
	// The suffix rule of the lookup answers "proxy/proxy.go" from the repository
	// path as well, so the question is asked of the rows themselves.
	var subRows int
	if err := s.DB().QueryRow(`SELECT count(*) FROM project_scan_files WHERE project = ? AND path = ?`,
		root, "proxy/proxy.go").Scan(&subRows); err != nil {
		t.Fatalf("count subordinate rows: %v", err)
	}
	if subRows != 0 {
		t.Error("the subordinate scan must not write rows of its own")
	}

	row, err := s.GetProjectScan(root)
	if err != nil || row == nil {
		t.Fatalf("the stored document must survive: row=%v err=%v", row, err)
	}
	if row.ScanJSON != repoDoc {
		t.Error("the repository document must not be replaced by a subordinate one")
	}
}

// A linked worktree is a checkout of its own and keeps its scan under its own
// key (YM-453). The guard must not swallow that case: the worktree scan is
// written, and the main checkout's index stays what it was.
func TestLos61dWorktreeRootStaysAllowed(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	main := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(main, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	worktree := filepath.Join(main, ".worktrees", "los61d")
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir worktree .git: %v", err)
	}
	// A linked worktree carries git data of its own, so it is its own checkout.
	if err := os.WriteFile(filepath.Join(worktree, ".git", "HEAD"), []byte("ref: refs/heads/yesloop/los-61d\n"), 0o644); err != nil {
		t.Fatalf("write worktree HEAD: %v", err)
	}

	mainDoc := los61dScanDoc(t, main, "internal/proxy/proxy.go")
	if err := s.PersistScan(main, mainDoc, "head1", 0); err != nil {
		t.Fatalf("persist main scan: %v", err)
	}
	if err := s.PersistScan(worktree, los61dScanDoc(t, worktree, "internal/storage/store.go"), "head1", 0); err != nil {
		t.Fatalf("a worktree scan must stay allowed under its own key: %v", err)
	}

	if !s.IsFileInCodeIndex(worktree, "internal/storage/store.go") {
		t.Error("the worktree scan must reach an index of its own")
	}
	row, err := s.GetProjectScan(main)
	if err != nil || row == nil {
		t.Fatalf("the main checkout's row must survive: row=%v err=%v", row, err)
	}
	if row.ScanJSON != mainDoc {
		t.Error("the worktree scan replaced the main checkout's document")
	}
}

// A .git entry is not a checkout. An empty directory of that name sits in plenty of
// working copies, and accepting it would hand the subtree scan the way past the guard
// that this test exists for.
func TestLos61dEmptyGitDirectoryIsNoCheckout(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	root := t.TempDir()
	sub := filepath.Join(root, "internal")
	if err := os.MkdirAll(filepath.Join(sub, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir empty .git: %v", err)
	}
	if err := s.PersistScan(root, los61dScanDoc(t, root, "internal/proxy/proxy.go"), "head1", 0); err != nil {
		t.Fatalf("persist repo scan: %v", err)
	}

	err := s.PersistScan(root, los61dScanDoc(t, sub, "proxy/proxy.go"), "head1", 0)

	if !errors.Is(err, ErrScanRootBelowCheckout) {
		t.Fatalf("an empty .git directory must not count as a checkout, got err = %v", err)
	}
	if !s.IsFileInCodeIndex(root, "internal/proxy/proxy.go") {
		t.Error("the repository index must keep its paths")
	}
}

// A document written by an older build can be rootless; it is the shape the
// index had before RootDir was carried, so it keeps replacing.
func TestLos61dRootlessDocumentStillReplaces(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	root := t.TempDir()
	if err := s.PersistScan(root, `{"Packages":[{"Files":[{"Path":"a.go"}]}]}`, "head1", 0); err != nil {
		t.Fatalf("persist rootless scan: %v", err)
	}
	if err := s.PersistScan(root, `{"Packages":[{"Files":[{"Path":"b.go"}]}]}`, "head2", 0); err != nil {
		t.Fatalf("persist second rootless scan: %v", err)
	}
	if s.IsFileInCodeIndex(root, "a.go") {
		t.Error("a rootless document keeps the old replace semantics")
	}
	if !s.IsFileInCodeIndex(root, "b.go") {
		t.Error("a rootless document keeps the old replace semantics")
	}
}

// The backfill runs on the hook hot path. A stored subordinate document must not
// turn into an index, and must not make the hook re-parse the document on every
// call either — the marker row is what ends the retry.
func TestLos61dBackfillRefusesSubordinateDocument(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	root := t.TempDir()
	subDoc := los61dScanDoc(t, filepath.Join(root, "internal"), "proxy/proxy.go")
	if _, err := s.DB().Exec(`INSERT INTO project_scan (project, scan_json, git_head, scanned_at)
		VALUES (?, ?, 'head1', CURRENT_TIMESTAMP)`, root, subDoc); err != nil {
		t.Fatalf("seed scan row: %v", err)
	}

	if s.IsFileInCodeIndex(root, "internal/proxy/proxy.go") {
		t.Fatal("a subordinate document must not index repository paths")
	}

	var indexed, marked int
	if err := s.DB().QueryRow(`SELECT count(*) FROM project_scan_files WHERE project = ? AND path <> ''`, root).Scan(&indexed); err != nil {
		t.Fatalf("count indexed paths: %v", err)
	}
	if indexed != 0 {
		t.Errorf("the backfill must not materialize a subordinate document, indexed = %d", indexed)
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM project_scan_files WHERE project = ? AND path = ''`, root).Scan(&marked); err != nil {
		t.Fatalf("count marker: %v", err)
	}
	if marked != 1 {
		t.Errorf("the backfill must mark the index materialized so the hook stops retrying, marker = %d", marked)
	}

	// The document is what would serve the next reader a subtree, so the backfill
	// drops it: the next scan then writes a repository-wide one.
	row, err := s.GetProjectScan(root)
	if err != nil {
		t.Fatalf("read scan row: %v", err)
	}
	if row != nil {
		t.Errorf("the subordinate document must be dropped, still stored: %q", row.ScanJSON)
	}
}

func los61dScanDoc(t *testing.T, rootDir string, paths ...string) string {
	t.Helper()
	type file struct {
		Path string
	}
	type pkg struct {
		Files []file
	}
	doc := struct {
		RootDir  string
		Packages []pkg
	}{RootDir: rootDir, Packages: []pkg{{}}}
	for _, p := range paths {
		doc.Packages[0].Files = append(doc.Packages[0].Files, file{Path: p})
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal scan document: %v", err)
	}
	return string(data)
}

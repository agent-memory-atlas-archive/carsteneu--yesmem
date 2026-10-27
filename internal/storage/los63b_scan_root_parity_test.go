package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/codescan"
)

// los63bGitWorktree builds a main checkout with a linked worktree the way git
// creates one: the worktree root carries a .git file that points at the main
// checkout's git data.
func los63bGitWorktree(t *testing.T) (main, worktree string) {
	t.Helper()
	main = t.TempDir()
	gitDir := filepath.Join(main, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "worktrees", "los63b"), 0o755); err != nil {
		t.Fatalf("mkdir main git data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write main HEAD: %v", err)
	}
	worktree = filepath.Join(main, ".worktrees", "los63b")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	link := "gitdir: " + filepath.Join(gitDir, "worktrees", "los63b") + "\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(link), 0o644); err != nil {
		t.Fatalf("write worktree gitfile: %v", err)
	}
	return main, worktree
}

// YM-453: a scan taken in a linked worktree describes another tree than the main
// checkout. Stored under the main checkout's key it replaced that index across
// restarts, so the hook blocked on a foreign file list and the wiki rendered
// another branch's code.
func TestLos63bWorktreeScanDoesNotReplaceMainIndex(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	main, worktree := los63bGitWorktree(t)
	mainKey, worktreeKey := codescan.ScanCacheKey(main), codescan.ScanCacheKey(worktree)
	if mainKey == worktreeKey {
		t.Fatalf("a worktree scan is filed under the main checkout's key %q", mainKey)
	}

	mainDoc := los61dScanDoc(t, main, "internal/proxy/proxy.go")
	if err := s.PersistScan(mainKey, mainDoc, "head1", 0); err != nil {
		t.Fatalf("persist main scan: %v", err)
	}
	worktreeDoc := los61dScanDoc(t, worktree, "internal/storage/store.go")
	if err := s.PersistScan(worktreeKey, worktreeDoc, "worktree-head", 0); err != nil {
		t.Fatalf("persist worktree scan: %v", err)
	}

	row, err := s.GetProjectScan(mainKey)
	if err != nil || row == nil {
		t.Fatalf("the main checkout's row must survive: row=%v err=%v", row, err)
	}
	if row.ScanJSON != mainDoc {
		t.Error("the worktree scan replaced the main checkout's document")
	}
	if row.GitHead != "head1" {
		t.Errorf("main checkout git head = %q, want head1", row.GitHead)
	}
	if !s.IsFileInCodeIndex(mainKey, "internal/proxy/proxy.go") {
		t.Error("the main checkout's index lost its paths")
	}
	if s.IsFileInCodeIndex(mainKey, "internal/storage/store.go") {
		t.Error("the worktree's file list reached the main checkout's index")
	}
	if !s.IsFileInCodeIndex(worktreeKey, "internal/storage/store.go") {
		t.Error("the worktree scan did not reach an index of its own")
	}
}

// YM-453: the two guards answer one question — may this document be the index
// the key names. Whatever codescan says, storage must say the same; the
// worktree case stays allowed, because a worktree is a checkout of its own.
func TestLos63bScanRootGuardMatchesCodescanRule(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	main, worktree := los63bGitWorktree(t)
	sub := filepath.Join(main, "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir subtree: %v", err)
	}

	cases := []struct {
		name string
		key  string
		root string
	}{
		{"checkout scan under its own key", codescan.ScanCacheKey(main), main},
		{"worktree scan under its own key", codescan.ScanCacheKey(worktree), worktree},
		{"worktree scan under the main key", codescan.ScanCacheKey(main), worktree},
		{"subtree scan under the checkout key", codescan.ScanCacheKey(main), sub},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := los61dScanDoc(t, tc.root, "a.go")
			err := s.PersistScan(tc.key, doc, "head1", 0)
			allowed := codescan.ScannedRepositoryRoot(tc.key, tc.root)
			if allowed != (err == nil) {
				t.Fatalf("codescan allows=%v, storage err=%v — the guards disagree", allowed, err)
			}
			if !allowed && !errors.Is(err, ErrScanRootBelowCheckout) {
				t.Fatalf("refused with the wrong error: %v", err)
			}
		})
	}
}

// YM-453: the row the bug left behind, a worktree document under the main
// checkout's key, must not become that checkout's index. The backfill that
// materializes the per-path index drops it, so the next scan of the checkout
// writes a repository-wide document instead.
func TestLos63bUpgradeDropsAWorktreeDocumentUnderTheMainKey(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	main, worktree := los63bGitWorktree(t)
	key := codescan.ScanCacheKey(main)
	doc := los61dScanDoc(t, worktree, "internal/storage/store.go")
	if _, err := s.DB().Exec(`INSERT INTO project_scan (project, scan_json, git_head, scanned_at)
		VALUES (?, ?, 'head1', CURRENT_TIMESTAMP)`, key, doc); err != nil {
		t.Fatalf("seed scan row: %v", err)
	}

	if s.IsFileInCodeIndex(key, "internal/storage/store.go") {
		t.Fatal("a foreign document must not index the main checkout")
	}
	row, err := s.GetProjectScan(key)
	if err != nil {
		t.Fatalf("read scan row: %v", err)
	}
	if row != nil {
		t.Errorf("the foreign document must be dropped, still stored: %q", row.ScanJSON)
	}
}

// A row without a key keeps the old tolerance: the per-path backfill meets such
// rows from the retired basename scheme, and refusing them would leave an index
// it can never replace.
func TestLos63bKeylessRowStillReplaces(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()

	root := t.TempDir()
	if err := s.PersistScan("", los61dScanDoc(t, root, "a.go"), "head1", 0); err != nil {
		t.Fatalf("a row without a key keeps the old tolerance: %v", err)
	}
}

package indexer

import (
	"os"
	"path/filepath"
	"testing"
)

// collectSessionFiles decides what gets indexed. A symlink inside the sessions
// tree must not add a file from outside it (YM-277 / DEV-DT45).
func TestCollectSessionFiles_SkipsSymlinks(t *testing.T) {
	idx, _ := setupIndexer(t)

	root := t.TempDir()
	own := filepath.Join(root, "own.jsonl")
	if err := os.WriteFile(own, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	foreign := filepath.Join(outside, "foreign.jsonl")
	if err := os.WriteFile(foreign, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, filepath.Join(root, "linked.jsonl")); err != nil {
		t.Fatal(err)
	}

	files, err := idx.collectSessionFiles(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(files) != 1 || files[0] != own {
		t.Fatalf("collected %v, want only %q", files, own)
	}
}

// The same at the level that matters: a session file reachable only through a
// symlink must not enter the session index.
func TestIndexAll_DoesNotFollowSymlinkedSessionFiles(t *testing.T) {
	idx, store := setupIndexer(t)

	fixture := filepath.Join("..", "parser", "testdata", "sample-session.jsonl")
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	// A relative target would dangle relative to the link's directory.
	target, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "linked.jsonl")); err != nil {
		t.Fatal(err)
	}

	indexed, _, err := idx.IndexAll(root)
	if err != nil {
		t.Fatalf("index all: %v", err)
	}
	if indexed != 0 {
		t.Fatalf("indexed %d session file(s) reached through a symlink", indexed)
	}

	sess, err := store.GetSession("test-session-001")
	if err == nil && sess != nil {
		t.Fatal("a session reachable only through a symlink was indexed")
	}
}

// The daemon's file watcher submits changed paths directly, without the walk,
// so the rejection has to sit on the read path as well: a symlink parked in the
// sessions tree must not index a foreign file (YM-277).
func TestIndexSession_RefusesASymlinkedSessionFile(t *testing.T) {
	idx, store := setupIndexer(t)

	fixture := filepath.Join("..", "parser", "testdata", "sample-session.jsonl")
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("fixture not found: %v", err)
	}
	target, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(t.TempDir(), "linked.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := idx.IndexSession(link); err == nil {
		t.Fatal("a symlinked session file was accepted")
	}
	if sess, err := store.GetSession("test-session-001"); err == nil && sess != nil {
		t.Fatal("a session reachable only through a symlink was indexed")
	}
}

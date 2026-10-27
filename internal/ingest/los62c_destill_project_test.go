package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los62cFakeLLM answers the two destillation passes in order: the filter pass
// returns specialist text, the structure pass returns one learning.
type los62cFakeLLM struct{ calls int }

func (f *los62cFakeLLM) Complete(string, string, ...extraction.CallOption) (string, error) {
	return "", nil
}

func (f *los62cFakeLLM) CompleteJSON(string, string, map[string]any, ...extraction.CallOption) (string, error) {
	f.calls++
	if f.calls == 1 {
		return `{"specialist_points":"` + strings.Repeat("a specialist detail from the docs ", 8) + `"}`, nil
	}
	return `{"domain":"code","learnings":[{"category":"gotcha","content":"a documented behaviour that surprises a reader","context":"the reference docs","trigger":"when reading the docs"}]}`, nil
}

func (f *los62cFakeLLM) Name() string  { return "los62c-fake" }
func (f *los62cFakeLLM) Model() string { return "los62c-fake-1" }

// TestLos62c_DestillChunks_CanonicalizesWorktreeProject pins finding 5 of YM-445.
// insertLearnings stamped the raw cfg.Project, while UpsertDocSource
// canonicalizes doc_sources.project through repo.RootOrSelf. An ingest started
// in a checkout therefore wrote its learnings under the checkout path and its
// source under the repository root: every project-scoped read missed them, and a
// project-scoped delete left them active with a dead doc_chunk_ref. The worktree
// here sits OUTSIDE the repository tree, which is the shape `.git/worktrees`
// pointers produce and the one a path-prefix heuristic would not resolve.
func TestLos62c_DestillChunks_CanonicalizesWorktreeProject(t *testing.T) {
	base := t.TempDir()
	mainRepo := filepath.Join(base, "mainrepo")
	if err := os.MkdirAll(filepath.Join(mainRepo, ".git"), 0o755); err != nil {
		t.Fatalf("create main repo: %v", err)
	}
	gitDir := filepath.Join(mainRepo, ".git", "worktrees", "checkout")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("create worktree gitdir: %v", err)
	}
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatalf("write worktree .git file: %v", err)
	}

	store, err := storage.Open(filepath.Join(t.TempDir(), "los62c.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	chunks := []storage.DocChunk{{
		ID:           7,
		SourceFile:   "guide.md",
		SourceHash:   "los62c-hash",
		HeadingPath:  "Setup",
		Content:      strings.Repeat("The reference documents one behaviour that a reader would not expect. ", 4),
		TokensApprox: 200,
	}}

	cfg := Config{Project: checkout, Name: "los62c-docs", Domain: "code"}
	if _, err := DestillChunks(chunks, cfg, &los62cFakeLLM{}, store); err != nil {
		t.Fatalf("destill: %v", err)
	}

	var stamped int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM learnings WHERE source = 'docs_extracted'`).Scan(&stamped); err != nil {
		t.Fatalf("count destilled learnings: %v", err)
	}
	if stamped != 1 {
		t.Fatalf("destilled learnings = %d, want 1 (the fixture must reach the insert)", stamped)
	}

	var project string
	if err := store.DB().QueryRow(`SELECT project FROM learnings WHERE source = 'docs_extracted'`).Scan(&project); err != nil {
		t.Fatalf("read stamped project: %v", err)
	}
	if project != mainRepo {
		t.Errorf("learning project = %q, want the repository root %q", project, mainRepo)
	}

	// The user-visible consequence: a read scoped to the repository finds the
	// learning. Before the fix it was stamped with the checkout path and this
	// query returned nothing.
	visible, err := store.GetActiveLearnings("", mainRepo, "", "", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}
	if len(visible) != 1 {
		t.Errorf("learnings visible under %q = %d, want 1: the ingest stamp must match the canonical project", mainRepo, len(visible))
	}
}

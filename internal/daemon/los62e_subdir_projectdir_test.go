package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/codescan"
)

// YM-449 / DEV-DT73: project_dir comes from the client, and real callers send a
// subdirectory. The graph cache is keyed by the resolved directory, so a
// subdirectory built a subtree-only graph under its own key and scanned again on
// every process start instead of hitting the project's index.
func TestGetCodeGraph_NormalizesSubdirectoryProjectDir(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }

	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sub := filepath.Join(repo, "internal", "coder")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.go"), []byte("package coder\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.getCodeGraph("", map[string]any{"project_dir": repo})
	h.getCodeGraph("", map[string]any{"project_dir": sub})

	want := codescan.ResolveScanRoot(repo)
	if len(h.codeGraphs) != 1 {
		keys := make([]string, 0, len(h.codeGraphs))
		for k := range h.codeGraphs {
			keys = append(keys, k)
		}
		t.Fatalf("subdir project_dir built its own graph: %d cache keys %v, want only %q", len(h.codeGraphs), keys, want)
	}
	for k := range h.codeGraphs {
		if k != want {
			t.Errorf("graph cached under %q, want %q", k, want)
		}
	}
}

// A linked worktree keeps its own graph: CheckoutRoot stops at the worktree's
// .git file, so the normalization of a subdirectory never folds worktree
// content onto the main checkout's index (YM-449 against YM-232).
func TestGetCodeGraph_KeepsWorktreeAsItsOwnScanRoot(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }

	main := t.TempDir()
	if out, err := exec.Command("git", "-C", main, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", main, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	wt := filepath.Join(main, "wt")
	if out, err := exec.Command("git", "-C", main, "worktree", "add", "-q", wt).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	sub := filepath.Join(wt, "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	h.getCodeGraph("", map[string]any{"project_dir": wt})
	h.getCodeGraph("", map[string]any{"project_dir": sub})

	want := codescan.ResolveScanRoot(wt)
	if len(h.codeGraphs) != 1 {
		keys := make([]string, 0, len(h.codeGraphs))
		for k := range h.codeGraphs {
			keys = append(keys, k)
		}
		t.Fatalf("worktree subdir is not the worktree root: %d cache keys %v, want only %q", len(h.codeGraphs), keys, want)
	}
	for k := range h.codeGraphs {
		if k != want {
			t.Errorf("graph cached under %q, want the worktree root %q", k, want)
		}
	}
}

// YM-449 / DEV-DT73 (Cold-Review-Befund 1): the graph is keyed by the checkout
// root, so its file paths are repository-relative. A subdirectory call has to
// translate its walk-relative paths before asking the graph, otherwise every hit
// stays "(unresolved)" and the enrichment is silently lost.
func TestHandleSearchCode_SubdirProjectDirEnrichesFromRepoGraph(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }

	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sub := filepath.Join(repo, "internal", "coder")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.go"), []byte("package coder\n\nfunc BTarget() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := h.handleSearchCode(map[string]any{"pattern": "BTarget", "project_dir": sub})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if strings.Contains(string(resp.Result), "(unresolved)") {
		t.Fatalf("the repository graph was not consulted for a subdirectory call:\n%s", resp.Result)
	}
	if !strings.Contains(string(resp.Result), "BTarget") {
		t.Fatalf("the match is missing:\n%s", resp.Result)
	}
}

// The graph now covers the whole checkout, but the walk must keep the caller's
// directory: a subdirectory argument is a scope, not a hint (DEV-DT73).
func TestHandleSearchCode_SubdirProjectDirStaysInsideTheSubdir(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }

	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sub := filepath.Join(repo, "inside")
	outside := filepath.Join(repo, "outside")
	for _, d := range []string{sub, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(sub, "a.go"), filepath.Join(outside, "b.go")} {
		if err := os.WriteFile(f, []byte("package p\n\n// needle_scope\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	resp := h.handleSearchCode(map[string]any{"pattern": "needle_scope", "project_dir": sub})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "a.go") {
		t.Fatalf("the match inside the subdirectory is missing:\n%s", resp.Result)
	}
	if strings.Contains(string(resp.Result), "b.go") {
		t.Fatalf("a subdirectory scope leaked a match from outside it:\n%s", resp.Result)
	}
}

// The same offset applies on the read side: a node's file is repository-relative,
// so resolving it against the subdirectory produced "<sub>/<sub>/<file>".
func TestHandleGetCodeSnippet_SubdirProjectDirReadsSymbolFile(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }

	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sub := filepath.Join(repo, "internal", "coder")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.go"), []byte("package coder\n\nfunc BTarget() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := h.handleGetCodeSnippet(map[string]any{"qualified_name": "BTarget", "project_dir": sub})
	if resp.Error != "" {
		t.Fatalf("a symbol of the project was not readable from a subdirectory call: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "func BTarget") {
		t.Fatalf("the symbol body is missing:\n%s", resp.Result)
	}
}

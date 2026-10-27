package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/codescan"
)

// YM-199 / DEV-DT35: file and dir come from the request, and filepath.Join
// resolves ".." — so a caller could read any file the daemon user can read.
// These tests sit on both entry points that take a client path: the file/dir
// parameters and a symlink that points out of the project.
func TestHandleGetCodeSnippet_RejectsPathOutsideProject(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package secret\n\nfunc Leak() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rel := filepath.Join("..", filepath.Base(outside), "secret.go")
	resp := h.handleGetCodeSnippet(map[string]any{
		"project":    project,
		"file":       rel,
		"start_line": float64(1),
		"end_line":   float64(3),
	})

	if resp.Error == "" {
		t.Fatalf("get_code_snippet escaped the project and read %s: %s", rel, resp.Result)
	}
	if strings.Contains(string(resp.Result), "func Leak()") {
		t.Errorf("leaked the outside file content: %s", resp.Result)
	}
}

func TestHandleGetFileSymbols_RejectsPathOutsideProject(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package secret\n\nfunc Leak() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rel := filepath.Join("..", filepath.Base(outside), "secret.go")
	resp := h.handleGetFileSymbols(map[string]any{"project": project, "file": rel})

	if resp.Error == "" {
		t.Fatalf("get_file_symbols escaped the project and read %s: %s", rel, resp.Result)
	}
	if strings.Contains(string(resp.Result), "Leak") {
		t.Errorf("leaked the outside file symbols: %s", resp.Result)
	}
}

func TestHandleGetCodeSnippet_RejectsAbsolutePathOutsideProject(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(target, []byte("package secret\n\nfunc Leak() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	resp := h.handleGetCodeSnippet(map[string]any{
		"project":    project,
		"file":       target,
		"start_line": float64(1),
		"end_line":   float64(3),
	})

	if resp.Error == "" {
		t.Fatalf("get_code_snippet accepted an absolute path outside the project: %s", resp.Result)
	}
}

// A symlink inside the project is the indirect route: the path looks contained,
// but the resolved target is not.
func TestHandleGetFileSymbols_RejectsSymlinkEscapingProject(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(target, []byte("package secret\n\nfunc Leak() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(project, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	resp := h.handleGetFileSymbols(map[string]any{"project": project, "file": "link.go"})

	if resp.Error == "" {
		t.Fatalf("get_file_symbols followed a symlink out of the project: %s", resp.Result)
	}
	if strings.Contains(string(resp.Result), "Leak") {
		t.Errorf("leaked symbols through the symlink: %s", resp.Result)
	}
}

func TestHandleGetFileIndex_RejectsDirOutsideProject(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package secret\n\nfunc Leak() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rel := filepath.Join("..", filepath.Base(outside))
	resp := h.handleGetFileIndex(map[string]any{"project": project, "dir": rel})

	if strings.Contains(string(resp.Result), "secret.go") {
		t.Fatalf("get_file_index listed files outside the project: %s", resp.Result)
	}
}

// The contained case must keep working: a sibling-relative path that stays inside
// the project is a legitimate range read.
func TestHandleGetCodeSnippet_AllowsPathInsideProject(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "pkg", "a.go"), []byte("package pkg\n\nfunc Kept() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	resp := h.handleGetCodeSnippet(map[string]any{
		"project":    project,
		"file":       filepath.Join("pkg", "a.go"),
		"start_line": float64(1),
		"end_line":   float64(3),
	})

	if resp.Error != "" {
		t.Fatalf("a contained path must still be readable: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "func Kept()") {
		t.Errorf("expected the file content, got: %s", resp.Result)
	}
}

// YM-224 / DEV-DT39: search_code walks the project and reads every source file
// it finds. filepath.Walk reports a symlink as its own entry, so a link that
// points out of the project is read by os.ReadFile right through the boundary —
// the walk-side twin of the escape YM-199 closed for client-supplied paths.
func TestHandleSearchCode_DoesNotFollowSymlinkOutOfProject(t *testing.T) {
	h, _ := mustHandler(t)
	// Pin the scanner: the graph build must not reach for a real CBM index.
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }

	project := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "outside.go")
	if err := os.WriteFile(target, []byte("package outside\n\n// needle_outside_project\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "inside.go"), []byte("package inside\n\n// needle_inside_project\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(project, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// An in-project symlink is the positive control: the rule is containment,
	// not a blanket skip of links.
	if err := os.Symlink(filepath.Join(project, "inside.go"), filepath.Join(project, "alias.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	resp := h.handleSearchCode(map[string]any{"pattern": "needle_", "project": project})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	if err := json.Unmarshal(resp.Result, &data); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.Contains(data["text"], "needle_outside_project") {
		t.Errorf("search_code followed a symlink out of the project:\n%s", data["text"])
	}
	if !strings.Contains(data["text"], "needle_inside_project") {
		t.Errorf("search_code stopped finding files inside the project:\n%s", data["text"])
	}
	if !strings.Contains(data["text"], "alias.go") {
		t.Errorf("search_code dropped a symlink that stays inside the project:\n%s", data["text"])
	}
}

// Security review (YM-224): the containment check is only as good as the root it
// compares against. resolveProjectDir hands back project_dir as the client sent
// it, so a relative root makes filepath.Walk produce paths that already carry the
// root name. Joining those onto the absolute root a second time leaves a path
// that does not exist, the symlink is never resolved, and the check passes.
func TestHandleSearchCode_RelativeProjectDirKeepsContainment(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }

	parent := t.TempDir()
	project := filepath.Join(parent, "proj")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "outside.go")
	if err := os.WriteFile(target, []byte("package outside\n\n// needle_outside_project\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "inside.go"), []byte("package inside\n\n// needle_inside_project\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(project, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// A relative project_dir is walked relative to the process working
	// directory; that is the shape the escape needs in production.
	t.Chdir(parent)

	resp := h.handleSearchCode(map[string]any{"pattern": "needle_", "project_dir": "proj"})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var data map[string]string
	if err := json.Unmarshal(resp.Result, &data); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.Contains(data["text"], "needle_outside_project") {
		t.Errorf("a relative project_dir defeated the containment check:\n%s", data["text"])
	}
	if !strings.Contains(data["text"], "needle_inside_project") {
		t.Errorf("search_code stopped finding files inside the project:\n%s", data["text"])
	}
}

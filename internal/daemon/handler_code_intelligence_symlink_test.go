package daemon

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/codescan"
)

// los04dFailingScanner forces the graph build into its error branch so the log
// line can be inspected without a real indexing backend.
type los04dFailingScanner struct{}

func (los04dFailingScanner) Scan(string) (*codescan.ScanResult, error) {
	return nil, errors.New("los04d: forced scan failure")
}

// YM-232 / DEV-DT42: get_file_index walked the project and listed every source
// file the walk reported, including a symlink whose target lies outside the
// project. Same walk-side rule as search_code (YM-224).
func TestHandleGetFileIndex_DoesNotListSymlinkOutOfProject(t *testing.T) {
	h, _ := mustHandler(t)
	project := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(target, []byte("package secret\n\nfunc Leak() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "inside.go"), []byte("package inside\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(project, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(project, "inside.go"), filepath.Join(project, "alias.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The dir parameter walks a subtree instead of the project root, and the
	// same containment rule has to hold there.
	sub := filepath.Join(project, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "kept.go"), []byte("package sub\n\nfunc B() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(sub, "out.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	resp := h.handleGetFileIndex(map[string]any{"project": project})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	text := string(resp.Result)
	if strings.Contains(text, "link.go") {
		t.Errorf("get_file_index listed a symlink out of the project:\n%s", text)
	}
	if !strings.Contains(text, "inside.go") {
		t.Errorf("get_file_index stopped listing files inside the project:\n%s", text)
	}
	if !strings.Contains(text, "alias.go") {
		t.Errorf("get_file_index dropped a symlink that stays inside the project:\n%s", text)
	}

	subResp := h.handleGetFileIndex(map[string]any{"project": project, "dir": "sub"})
	if subResp.Error != "" {
		t.Fatalf("unexpected error for the dir parameter: %s", subResp.Error)
	}
	subText := string(subResp.Result)
	if strings.Contains(subText, "out.go") {
		t.Errorf("get_file_index listed a symlink out of the project below dir=sub:\n%s", subText)
	}
	if !strings.Contains(subText, "kept.go") {
		t.Errorf("get_file_index stopped listing files below dir=sub:\n%s", subText)
	}
}

// YM-232: a project root handed in as a symlink produced an empty listing,
// because filepath.Walk never enters a symlinked root.
func TestHandleGetFileIndex_SymlinkedProjectRootListsFiles(t *testing.T) {
	h, _ := mustHandler(t)
	parent := t.TempDir()
	project := filepath.Join(parent, "proj")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(project, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	resp := h.handleGetFileIndex(map[string]any{"project": link})
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if !strings.Contains(string(resp.Result), "a.go") {
		t.Errorf("a symlinked project root produced an empty listing:\n%s", resp.Result)
	}
}

// YM-232: the same root shape, and the dot form, on the file-content path.
func TestHandleSearchCode_SymlinkedAndDotProjectRootFindFiles(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }

	parent := t.TempDir()
	project := filepath.Join(parent, "proj")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "inside.go"), []byte("package inside\n\n// needle_inside_project\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(project, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, root := range []string{link, project} {
		resp := h.handleSearchCode(map[string]any{"pattern": "needle_", "project": root})
		if resp.Error != "" {
			t.Fatalf("root %q: unexpected error: %s", root, resp.Error)
		}
		if !strings.Contains(string(resp.Result), "needle_inside_project") {
			t.Errorf("root %q found nothing:\n%s", root, resp.Result)
		}
	}

	t.Chdir(project)
	resp := h.handleSearchCode(map[string]any{"pattern": "needle_", "project_dir": "."})
	if resp.Error != "" {
		t.Fatalf(`root ".": unexpected error: %s`, resp.Error)
	}
	if !strings.Contains(string(resp.Result), "needle_inside_project") {
		t.Errorf(`root "." found nothing:\n%s`, resp.Result)
	}
}

// YM-232 (consequence check): the graph cache is keyed by the resolved project
// directory, so a project reached once by link and once by its real path must
// not end up under two keys.
func TestGetCodeGraph_SharesOneEntryForSymlinkedAndRealRoot(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return &codescan.DirectoryScanner{} }

	parent := t.TempDir()
	real := filepath.Join(parent, "proj")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	h.getCodeGraph("", map[string]any{"project_dir": link})
	h.getCodeGraph("", map[string]any{"project_dir": real})

	if len(h.codeGraphs) != 1 {
		keys := make([]string, 0, len(h.codeGraphs))
		for k := range h.codeGraphs {
			keys = append(keys, k)
		}
		t.Errorf("one project cached under %d keys: %v", len(h.codeGraphs), keys)
	}
}

// YM-232: the canonicalization contract itself. A symlinked project root has to
// resolve to its real path, and the dot form has to become that same absolute
// path instead of staying relative to the daemon's working directory.
func TestResolveProjectDir_ReturnsCanonicalPath(t *testing.T) {
	h, _ := mustHandler(t)
	parent := t.TempDir()
	real := filepath.Join(parent, "proj")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	if got := h.resolveProjectDir("", map[string]any{"project_dir": link}); got != want {
		t.Errorf("symlinked project_dir resolved to %q, want %q", got, want)
	}
	t.Chdir(real)
	if got := h.resolveProjectDir("", map[string]any{"project_dir": "."}); got != want {
		t.Errorf(`project_dir "." resolved to %q, want %q`, got, want)
	}
}

// DEV-DT42 (4): the scan-failure log line printed the client-supplied project
// directory unquoted, so a path carrying a newline forged its own log line.
func TestGetCodeGraph_QuotesClientPathInLogLine(t *testing.T) {
	h, _ := mustHandler(t)
	h.codeScanner = func() codescan.Scanner { return los04dFailingScanner{} }

	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })

	forged := filepath.Join(t.TempDir(), "forged\nLOG LINE: everything is fine")
	if graph := h.getCodeGraph("", map[string]any{"project_dir": forged}); graph != nil {
		t.Fatalf("expected no graph after a failing scan")
	}

	out := buf.String()
	if !strings.Contains(out, "los04d: forced scan failure") {
		t.Fatalf("the scan failure never reached the log: %q", out)
	}
	if strings.Contains(out, "forged\nLOG LINE") {
		t.Errorf("client path reached the log unquoted (forged line): %q", out)
	}
	if !strings.Contains(out, `forged\nLOG LINE`) {
		t.Errorf("expected the client path quoted in the log line: %q", out)
	}
}

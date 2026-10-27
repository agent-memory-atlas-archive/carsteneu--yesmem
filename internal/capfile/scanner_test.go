package capfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeBashHandlerCap(name, desc string) *CapFile {
	return &CapFile{
		Name: name, Description: desc, Version: 1, Scope: "user", Purpose: "Test.",
		Scripts: []Script{{
			Name: name, Kind: "handler", Runtime: "bash", Lang: "bash",
			Body: "echo ok",
		}},
	}
}

func makeREPLToolCap(name, desc string) *CapFile {
	return &CapFile{
		Name: name, Description: desc, Version: 1, Scope: "user", Purpose: "Test.",
		Scripts: []Script{{
			Name: name, Kind: "tool", Runtime: "repl", Lang: "javascript",
			Body: "async ({ x }) => x",
		}},
	}
}

func TestScanDirectory_FindsCAPFiles(t *testing.T) {
	dir := t.TempDir()

	cap1 := makeBashHandlerCap("cap_a", "A")
	cap2 := makeREPLToolCap("cap_b", "B")

	if err := WriteFile(cap1, filepath.Join(dir, "cap_a")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(cap2, filepath.Join(dir, "cap_b")); err != nil {
		t.Fatal(err)
	}

	caps, errs := ScanDirectory(dir)
	if len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	if len(caps) != 2 {
		t.Fatalf("want 2 caps, got %d", len(caps))
	}

	names := map[string]bool{}
	for _, c := range caps {
		names[c.Name] = true
	}
	if !names["cap_a"] || !names["cap_b"] {
		t.Errorf("want cap_a and cap_b, got %v", names)
	}
}

func TestScanDirectory_SkipsInvalidDirs(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "bad_cap"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad_cap", "CAP.md"), []byte("not valid"), 0o644); err != nil {
		t.Fatal(err)
	}

	cap1 := makeBashHandlerCap("good_cap", "Good")
	if err := WriteFile(cap1, filepath.Join(dir, "good_cap")); err != nil {
		t.Fatal(err)
	}

	caps, errs := ScanDirectory(dir)
	if len(errs) != 1 {
		t.Errorf("want 1 error for bad_cap, got %d", len(errs))
	}
	if len(caps) != 1 {
		t.Fatalf("want 1 good cap, got %d", len(caps))
	}
	if caps[0].Name != "good_cap" {
		t.Errorf("want good_cap, got %q", caps[0].Name)
	}
}

func TestScanDirectory_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	caps, errs := ScanDirectory(dir)
	if len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	if len(caps) != 0 {
		t.Errorf("want 0 caps, got %d", len(caps))
	}
}

func TestScanDirectory_NonexistentDir(t *testing.T) {
	caps, errs := ScanDirectory("/tmp/nonexistent-caps-dir-12345")
	if len(errs) > 0 {
		t.Errorf("nonexistent dir should not error: %v", errs)
	}
	if len(caps) != 0 {
		t.Errorf("want 0 caps, got %d", len(caps))
	}
}

func TestScanDirectory_SetsSourcePath(t *testing.T) {
	dir := t.TempDir()
	cf := makeBashHandlerCap("path_cap", "Path test")
	if err := WriteFile(cf, filepath.Join(dir, "path_cap")); err != nil {
		t.Fatal(err)
	}

	caps, _ := ScanDirectory(dir)
	if len(caps) != 1 {
		t.Fatalf("want 1 cap, got %d", len(caps))
	}
	want := filepath.Join(dir, "path_cap", "CAP.md")
	if caps[0].SourcePath != want {
		t.Errorf("SourcePath: want %q, got %q", want, caps[0].SourcePath)
	}
}

func TestScanAll_MergesBothDirs(t *testing.T) {
	userDir := t.TempDir()
	projDir := t.TempDir()

	userCap := makeBashHandlerCap("user_cap", "User")
	projCap := makeBashHandlerCap("proj_cap", "Project")
	projCap.Scope = "project"

	if err := WriteFile(userCap, filepath.Join(userDir, "user_cap")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(projCap, filepath.Join(projDir, "proj_cap")); err != nil {
		t.Fatal(err)
	}

	caps, errs := ScanAll(userDir, projDir)
	if len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	if len(caps) != 2 {
		t.Fatalf("want 2 caps, got %d", len(caps))
	}
}

// YM-174/OPS-20: the scan took every CAP.md it found, whatever the directory
// was called. A stray copy like "telegram (Kopie)" therefore imported the
// original cap's name and bumped its version on every daemon restart.
func TestScanDirectory_RejectsDirectoryNameMismatch(t *testing.T) {
	dir := t.TempDir()

	cf := makeBashHandlerCap("telegram", "Telegram")
	if err := WriteFile(cf, filepath.Join(dir, "telegram")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(cf, filepath.Join(dir, "telegram (Kopie)")); err != nil {
		t.Fatal(err)
	}

	caps, errs := ScanDirectory(dir)
	if len(caps) != 1 || caps[0].SourcePath != filepath.Join(dir, "telegram", "CAP.md") {
		t.Fatalf("want only the directory that matches the frontmatter name, got %d caps", len(caps))
	}
	if len(errs) != 1 {
		t.Fatalf("want 1 error for the mismatched directory, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "telegram (Kopie)") {
		t.Errorf("error must name the offending directory, got %q", errs[0])
	}
}

// YM-210/OPS-43 (latent case from E8): a scope-less CAP.md below the project
// directory was read with the parser's user default, so the cap landed
// project-agnostic instead of project-bound.
func TestScanAll_ProjectDirScopeLessCapIsProjectScoped(t *testing.T) {
	userDir := t.TempDir()
	projDir := t.TempDir()

	cf := makeBashHandlerCap("proj_cap", "Project")
	cf.Scope = ""
	if err := WriteFile(cf, filepath.Join(projDir, "proj_cap")); err != nil {
		t.Fatal(err)
	}

	caps, errs := ScanAll(userDir, projDir)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(caps) != 1 {
		t.Fatalf("want 1 cap, got %d", len(caps))
	}
	if caps[0].Scope != "project" {
		t.Errorf("Scope = %q, want project — a scope-less CAP.md in the project dir is project-bound", caps[0].Scope)
	}
}

// The same scope-less file with sandbox=none must be refused: loading it as a
// project-agnostic, unsandboxed cap is exactly the gap E8 reported.
func TestScanAll_ProjectDirScopeLessSandboxNoneIsRefused(t *testing.T) {
	userDir := t.TempDir()
	projDir := t.TempDir()

	cf := makeBashHandlerCap("unsandboxed", "Runs unsandboxed")
	cf.Scope = ""
	cf.Scripts[0].Sandbox = "none"
	if err := WriteFile(cf, filepath.Join(projDir, "unsandboxed")); err != nil {
		t.Fatal(err)
	}

	caps, errs := ScanAll(userDir, projDir)
	if len(caps) != 0 {
		t.Fatalf("a scope-less project-dir cap with sandbox=none must not load, got %d caps", len(caps))
	}
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %d: %v", len(errs), errs)
	}
}

// A scope-less CAP.md below the user directory keeps the shareable default.
func TestScanAll_UserDirScopeLessCapStaysUserScoped(t *testing.T) {
	userDir := t.TempDir()
	projDir := t.TempDir()

	cf := makeBashHandlerCap("user_cap", "User")
	cf.Scope = ""
	if err := WriteFile(cf, filepath.Join(userDir, "user_cap")); err != nil {
		t.Fatal(err)
	}

	caps, errs := ScanAll(userDir, projDir)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(caps) != 1 {
		t.Fatalf("want 1 cap, got %d", len(caps))
	}
	if caps[0].Scope != "user" {
		t.Errorf("Scope = %q, want user", caps[0].Scope)
	}
}

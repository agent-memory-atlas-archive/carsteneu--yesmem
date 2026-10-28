package capfile

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-373 (OPS-93, key 2): the import path read CAP.md with os.ReadFile, which
// follows a symlink, while the export path already refused a link
// (WriteCapToDisk uses ReadNoFollow, YM-359). A link planted in an import
// directory therefore fed the version gate and the cap store content from a file
// outside the caps tree.
//
// The read is the same read on both paths, so it refuses the same things.
func TestLos56gScanDirectoryRefusesASymlinkedCapFile(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "planted.md")
	if err := WriteOwnerOnly(elsewhere, Render(makeBashHandlerCap("los56g_planted", "planted"))); err != nil {
		t.Fatalf("write the link target: %v", err)
	}
	capDir := filepath.Join(dir, "los56g_planted")
	if err := os.MkdirAll(capDir, 0o700); err != nil {
		t.Fatalf("mkdir cap dir: %v", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(capDir, "CAP.md")); err != nil {
		t.Fatalf("plant the link: %v", err)
	}

	caps, errs := ScanDirectory(dir)

	if len(caps) != 0 {
		t.Errorf("the scan imported %d cap(s) through a symlink, want 0", len(caps))
	}
	if len(errs) == 0 {
		t.Errorf("the refused link was silent, want an error naming the path")
	}
}

// A regular CAP.md next to it still imports: the refusal must not cost the
// directory its other caps.
func TestLos56gScanDirectoryKeepsRegularCapFiles(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "los56g_real")
	if err := WriteFile(makeBashHandlerCap("los56g_real", "real"), realDir); err != nil {
		t.Fatalf("write the cap: %v", err)
	}

	caps, errs := ScanDirectory(dir)

	if len(errs) != 0 {
		t.Errorf("a regular CAP.md reported errors: %v", errs)
	}
	if len(caps) != 1 || caps[0].Name != "los56g_real" {
		t.Errorf("the regular cap did not import, got %d cap(s)", len(caps))
	}
}

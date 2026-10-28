package capfile

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-353 (OPS-87, key 4): the file was written 0600 while the directory holding it
// stayed 0755. The directory entry alone names every installed cap and its mtime,
// so the file mode was guarding the content and not the inventory.
func TestLos35eWriteFileCreatesTheDirOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap_dir")
	if err := WriteFile(los35eProbeCap(), dir); err != nil {
		t.Fatalf("write cap file: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat cap dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("cap dir mode = %04o, want 0700: the listing is the inventory", perm)
	}
}

// MkdirAll applies its mode only when it creates the directory, so a cap directory
// written before this fix keeps 0755 forever without an explicit repair.
func TestLos35eWriteFileRepairsAnExistingDirMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap_dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir cap dir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod cap dir: %v", err)
	}

	if err := WriteFile(los35eProbeCap(), dir); err != nil {
		t.Fatalf("write cap file: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat cap dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("cap dir mode = %04o, want 0700: an existing directory keeps its mode", perm)
	}
}

func los35eProbeCap() *CapFile {
	return &CapFile{
		Name:        "los35e_probe",
		Description: "Test: directory mode",
		Version:     1,
		Scope:       "user",
		Scripts: []Script{
			{Name: "los35e_probe", Kind: "tool", Runtime: "bash", Body: "echo hi", Schema: "{}"},
		},
	}
}

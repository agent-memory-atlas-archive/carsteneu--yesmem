package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-353 (OPS-87, key 4): the abort paths the writer gained. Both exist because a
// cap row can arrive through remember() instead of save_cap, so neither the name
// nor the base dir is trustworthy by the time the writer sees them.
func TestLos35eWriteCapToDiskRejectsATraversalName(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(filepath.Dir(base), "los35e_escaped")

	err := WriteCapToDisk(los35eWriterCap("../../los35e_escaped"), "", base, nil, ProfileStandard)
	if err == nil {
		t.Fatal("WriteCapToDisk accepted a traversal-shaped cap name")
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Fatalf("the writer created %s, outside the caps dir", outside)
	}
}

func TestLos35eWriteCapToDiskRejectsARelativeBaseDir(t *testing.T) {
	if err := WriteCapToDisk(los35eWriterCap("los35e_relative"), "", "caps", nil, ProfileStandard); err == nil {
		t.Fatal("WriteCapToDisk accepted a relative caps dir, it resolves against the daemon cwd")
	}
}

// The skip branch is the path an existing install takes on every daemon start: the
// content is current, the mode is not. Without the repair a cap dir exported before
// YM-353 keeps 0755 forever.
func TestLos35eWriteCapToDiskRepairsALeafDirMode(t *testing.T) {
	base := t.TempDir()
	meta := los35eWriterCap("los35e_mode_repair")
	if err := WriteCapToDisk(meta, "", base, nil, ProfileStandard); err != nil {
		t.Fatalf("first write: %v", err)
	}
	dir := filepath.Join(base, meta.Name)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	if err := WriteCapToDisk(meta, "", base, nil, ProfileStandard); err != nil {
		t.Fatalf("second write: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("leaf dir mode = %04o, want 0700", perm)
	}
}

// The root listing names every installed cap too, and the installer is the only
// other place that repairs it: a daemon started on an install created before
// YM-353 never goes through the installer.
func TestLos35eExportAllCapsRepairsTheRootDirMode(t *testing.T) {
	isolateHome(t)
	h, _ := mustHandler(t)
	los35bInsertCap(t, h, "los35e_root_mode", t.TempDir(), "echo hi", "")

	base := filepath.Join(t.TempDir(), "caps")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir caps root: %v", err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatalf("chmod caps root: %v", err)
	}

	ExportAllCaps(h, base)

	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat caps root: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("caps root mode = %04o, want 0700", perm)
	}
}

func los35eWriterCap(name string) CapMeta {
	return CapMeta{
		Name:        name,
		Description: "test cap",
		Scripts:     []ScriptMeta{{Name: "run", Kind: "tool", Runtime: "bash", Body: "echo hi"}},
		Version:     1,
		Tested:      true,
		Scope:       "user",
	}
}

package capfile

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-346/OPS-85, key 4b: the exported CAP.md carried 0644. It lands in the
// directory the daemon imports scripts from, and a cap body is operational
// detail — endpoints, directory layouts, command lines — that no other account on
// the host has any business reading. The file is only ever read by its owner.
func TestLos35cWriteFileIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "owner_only")
	cf := &CapFile{
		Name:        "owner_only",
		Description: "Test: owner-only export",
		Version:     1,
		Scope:       "user",
		Scripts: []Script{
			{Name: "owner_only", Kind: "tool", Runtime: "bash", Body: "echo hi", Schema: "{}"},
		},
	}
	if err := WriteFile(cf, dir); err != nil {
		t.Fatalf("write cap file: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "CAP.md"))
	if err != nil {
		t.Fatalf("stat cap file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("CAP.md mode = %04o, want 0600: the body is operational detail, not public", perm)
	}
}

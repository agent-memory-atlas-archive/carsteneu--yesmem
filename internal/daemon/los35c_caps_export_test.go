package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-346/OPS-85, key 4a: save_cap wrote its CAP.md into a directory derived from
// the data dir, while the daemon imports (and the CapsDirWatcher scans) the
// configured caps_dir. With a configured caps_dir the two paths diverge, so the
// file a save just wrote was never read back — and a later export wrote the same
// cap into the other directory.
func TestLos35cSaveCapWritesIntoTheConfiguredCapsDir(t *testing.T) {
	h, _ := mustHandler(t)
	configured := t.TempDir()
	h.capsDir = configured

	resp := h.handleSaveCap(map[string]any{
		"name":         "los35c_export",
		"description":  "Test: export into the configured caps dir",
		"handler_bash": "echo exported",
		"scope":        "user",
	})
	if resp.Error != "" {
		t.Fatalf("save_cap: %s", resp.Error)
	}

	want := filepath.Join(configured, "los35c_export", "CAP.md")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("save_cap did not write %s: %v", want, err)
	}

	derived := filepath.Join(filepath.Dir(h.dataDir), "caps", "los35c_export", "CAP.md")
	if _, err := os.Stat(derived); err == nil {
		t.Errorf("save_cap still wrote the data-dir-derived path %s, which the daemon never imports", derived)
	}
}

// Without a configured caps_dir the fallback stays what it was: the caps
// directory beside the data dir.
func TestLos35cSaveCapFallsBackToTheDataDirCapsPath(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleSaveCap(map[string]any{
		"name":         "los35c_export_fallback",
		"description":  "Test: export fallback",
		"handler_bash": "echo exported",
		"scope":        "user",
	})
	if resp.Error != "" {
		t.Fatalf("save_cap: %s", resp.Error)
	}

	want := filepath.Join(filepath.Dir(h.dataDir), "caps", "los35c_export_fallback", "CAP.md")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("without a configured caps_dir the export belongs at %s: %v", want, err)
	}
}

// ExportAllCaps runs on every daemon start and skips a file whose disk version is
// at or above the row's. That skip must still repair the mode: a CAP.md exported
// before the 0600 rule never reaches capfile.WriteFile again.
func TestLos35cWriteCapToDiskRepairsTheModeOnTheSkipPath(t *testing.T) {
	dir := t.TempDir()
	meta := CapMeta{
		Name:    "los35c_mode",
		Scripts: []ScriptMeta{{Name: "run", Kind: "tool", Runtime: "bash", Body: "echo hi", Schema: "{}"}},
		Version: 5,
		Tested:  true,
		Scope:   "user",
	}
	if err := WriteCapToDisk(meta, "", dir, nil, ProfileStandard); err != nil {
		t.Fatalf("write cap to disk: %v", err)
	}
	path := filepath.Join(dir, "los35c_mode", "CAP.md")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := WriteCapToDisk(meta, "", dir, nil, ProfileStandard); err != nil {
		t.Fatalf("second write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode after the skip branch = %04o, want 0600", perm)
	}
}

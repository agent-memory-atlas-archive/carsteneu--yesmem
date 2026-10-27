package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// handleScratchpadDelete derives <dataDir>/projects/<project>/assets from the
// caller's project value and removes that directory recursively. project is an
// absolute repository path in normal use, so a basename check cannot be the
// guard: ".." segments have to be proven harmless against the projects root
// (OPS-68).
func TestHandleScratchpadDelete_RefusesProjectOutsideProjectsRoot(t *testing.T) {
	cases := []struct {
		name    string
		project string
		// canary is the directory the raw project value resolves to, relative
		// to dataDir. It must survive the call untouched.
		canary string
	}{
		{"one level up", "..", "assets"},
		{"two levels up", "../..", "../assets"},
		{"dot segments after a name", "x/../..", "assets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := mustHandler(t)

			canaryDir := filepath.Join(h.dataDir, filepath.FromSlash(tc.canary))
			if err := os.MkdirAll(canaryDir, 0o755); err != nil {
				t.Fatalf("mkdir canary dir: %v", err)
			}
			canary := filepath.Join(canaryDir, "keep.txt")
			if err := os.WriteFile(canary, []byte("keep"), 0o644); err != nil {
				t.Fatalf("write canary: %v", err)
			}

			resp := h.handleScratchpadDelete(map[string]any{
				"project": tc.project,
				"section": "",
			})

			if _, err := os.Stat(canary); err != nil {
				t.Fatalf("project %q reached outside the projects root: %s is gone (response error %q)", tc.project, canary, resp.Error)
			}
			if resp.Error == "" {
				t.Fatalf("project %q was accepted; expected a refusal", tc.project)
			}
		})
	}
}

// An absolute project path is the normal case: the assets directory below
// <dataDir>/projects must still be removed when the whole section is deleted.
func TestHandleScratchpadDelete_RemovesAssetsOfAbsoluteProject(t *testing.T) {
	h, _ := mustHandler(t)

	project := "/home/example/repo"
	assetsDir := filepath.Join(h.dataDir, "projects", project, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}

	resp := h.handleScratchpadDelete(map[string]any{"project": project})
	if resp.Error != "" {
		t.Fatalf("absolute project was refused: %s", resp.Error)
	}
	if _, err := os.Stat(assetsDir); !os.IsNotExist(err) {
		t.Fatalf("assets dir of %q still exists (stat err %v)", project, err)
	}
}

// A refused delete must not have deleted rows either: the guard runs before
// storage.ScratchpadDelete, which has no guard of its own.
func TestHandleScratchpadDelete_RefusalKeepsTheSection(t *testing.T) {
	h, _ := mustHandler(t)

	h.handleScratchpadWrite(map[string]any{
		"project": "..", "section": "briefing", "content": "keep me",
	})

	resp := h.handleScratchpadDelete(map[string]any{"project": ".."})
	if resp.Error == "" {
		t.Fatal("escaping project was accepted")
	}

	read := h.handleScratchpadRead(map[string]any{"project": ".."})
	sections, _ := resultMap(t, read)["sections"].([]any)
	if len(sections) != 1 {
		t.Fatalf("refused delete removed the rows: %d sections left", len(sections))
	}
	if got := sections[0].(map[string]any)["content"]; got != "keep me" {
		t.Fatalf("content changed: %#v", got)
	}
}

// The write path derives its mkdir from the same caller value, so it must not
// create a directory outside the projects root either.
func TestHandleScratchpadWrite_DoesNotCreateAssetsOutsideRoot(t *testing.T) {
	h, _ := mustHandler(t)

	resp := h.handleScratchpadWrite(map[string]any{
		"project": "..", "section": "status", "content": "x",
	})
	if resp.Error != "" {
		t.Fatalf("write failed: %s", resp.Error)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, "assets")); !os.IsNotExist(err) {
		t.Fatalf("write created an assets dir outside the projects root (stat err %v)", err)
	}
}

// An empty data dir has no projects root to stay inside.
func TestScratchpadAssetsDir_RejectsEmptyDataDir(t *testing.T) {
	h, _ := mustHandler(t)
	h.dataDir = ""

	if _, err := h.scratchpadAssetsDir("proj"); err == nil {
		t.Fatal("empty data dir was accepted")
	}
}

func TestHandleScratchpadDelete_SectionKeepsAssets(t *testing.T) {
	h, _ := mustHandler(t)

	project := "/home/example/repo"
	assetsDir := filepath.Join(h.dataDir, "projects", project, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}

	resp := h.handleScratchpadDelete(map[string]any{"project": project, "section": "status"})
	if resp.Error != "" {
		t.Fatalf("section delete failed: %s", resp.Error)
	}
	if _, err := os.Stat(assetsDir); err != nil {
		t.Fatalf("section delete removed the assets dir: %v", err)
	}
}

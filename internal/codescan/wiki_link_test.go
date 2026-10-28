package codescan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/wikidir"
)

func TestRenderWikiLink_FileMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := renderWikiLink("yesmem")
	if !strings.Contains(out, "~/.claude/yesmem/wiki/yesmem/") {
		t.Errorf("missing wiki path in: %q", out)
	}
	if !strings.Contains(out, "not yet rendered") {
		t.Errorf("missing 'not yet rendered' fallback in: %q", out)
	}
	if !strings.Contains(out, "BEFORE") {
		t.Errorf("missing BEFORE mandate in: %q", out)
	}
	if !strings.Contains(out, "check its wiki page") {
		t.Errorf("missing 'check its wiki page' in: %q", out)
	}
}

func TestRenderWikiLink_FilePresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wikiDir := filepath.Join(home, ".claude", "yesmem", "wiki", "yesmem")
	if err := os.MkdirAll(wikiDir, 0755); err != nil {
		t.Fatal(err)
	}
	healthPath := filepath.Join(wikiDir, "health.md")
	if err := os.WriteFile(healthPath, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	out := renderWikiLink("yesmem")
	if !strings.Contains(out, "last build:") {
		t.Errorf("missing timestamp label in: %q", out)
	}
	// Verify timestamp is recent (today, within the last hour)
	want := time.Now().Format("15:04")
	if !strings.Contains(out, want) {
		alt := time.Now().Add(-time.Minute).Format("15:04")
		if !strings.Contains(out, alt) {
			t.Errorf("missing recent timestamp in: %q (expected %s or %s)", out, want, alt)
		}
	}
	if !strings.Contains(out, "search_code_index") {
		t.Errorf("missing fallback hierarchy mention (MCP code tools) in: %q", out)
	}
	if !strings.Contains(out, "BEFORE") {
		t.Errorf("missing BEFORE mandate in: %q", out)
	}
}

// Regression YM-6: readers get the project as a path (the code map knows the
// repo root), the writer stores by short name. Both sides must resolve to
// wiki/<basename>/.
func TestRenderWikiLink_PathInputResolvesToShortName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := renderWikiLink("/home/tester/memory/yesmem")
	if !strings.Contains(out, "~/.claude/yesmem/wiki/yesmem/") {
		t.Errorf("expected short-name wiki path for a path input, got: %q", out)
	}
	if strings.Contains(out, "/home/tester/memory") {
		t.Errorf("wiki link leaks the full project path: %q", out)
	}
}

func TestRenderWikiLink_PathInputFindsHealthFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wikiDir := filepath.Join(home, ".claude", "yesmem", "wiki", "yesmem")
	if err := os.MkdirAll(wikiDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiDir, "health.md"), []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	out := renderWikiLink("/home/tester/memory/yesmem")
	if !strings.Contains(out, "last build:") {
		t.Errorf("expected health.md of the short-name dir to be found, got: %q", out)
	}
}

// Cold-review finding: the code-map header names the directory the writer
// rendered into — including the collision-suffixed one — when the markers are
// available under HOME.
func TestRenderWikiLink_FollowsCollisionMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude", "yesmem", "wiki")
	projects := []string{"/a/samename", "/z/samename"}
	names := wikidir.AssignNames(root, projects)
	for _, p := range projects {
		if err := wikidir.WriteMarker(filepath.Join(root, names[p]), p); err != nil {
			t.Fatal(err)
		}
	}
	loser := "/z/samename"
	if names[loser] == "samename" {
		t.Fatalf("test setup did not produce a collision: %v", names)
	}

	out := renderWikiLink(loser)
	if !strings.Contains(out, "wiki/"+names[loser]+"/") {
		t.Errorf("header did not follow the marker of the colliding project (want %s): %q", names[loser], out)
	}
}

func TestRenderWikiLink_EmptyProjectFallsBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := renderWikiLink("")
	if !strings.Contains(out, "wiki/project/") {
		t.Errorf("expected the project fallback directory, got: %q", out)
	}
}

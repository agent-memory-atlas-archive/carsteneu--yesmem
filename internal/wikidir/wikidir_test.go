package wikidir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortName(t *testing.T) {
	cases := []struct {
		name    string
		project string
		want    string
	}{
		{"absolute path", "/home/tester/memory/yesmem", "yesmem"},
		{"trailing slash", "/home/tester/memory/yesmem/", "yesmem"},
		{"nested path", "/home/tester/worktrees/agent-42", "agent-42"},
		{"short name stays", "yesmem", "yesmem"},
		{"sentinel stays", "__global__", "__global__"},
		{"empty", "", ""},
		{"root", "/", ""},
		{"dot", ".", ""},
		{"dotdot", "..", ""},
		{"escaping", "foo/../..", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShortName(tc.project); got != tc.want {
				t.Errorf("ShortName(%q) = %q, want %q", tc.project, got, tc.want)
			}
		})
	}
}

func TestDisplayDir(t *testing.T) {
	if got, want := DisplayDir("/home/tester/memory/yesmem"), "~/.claude/yesmem/wiki/yesmem"; got != want {
		t.Errorf("DisplayDir(path) = %q, want %q", got, want)
	}
	if got, want := DisplayDir("yesmem"), "~/.claude/yesmem/wiki/yesmem"; got != want {
		t.Errorf("DisplayDir(name) = %q, want %q", got, want)
	}
}

func TestRoot(t *testing.T) {
	if got, want := Root("/home/u/.claude/yesmem"), filepath.Join("/home/u/.claude/yesmem", "wiki"); got != want {
		t.Errorf("Root = %q, want %q", got, want)
	}
}

func TestAssignNames_DistinctBasenames(t *testing.T) {
	root := t.TempDir()
	got := AssignNames(root, []string{"/a/alpha", "/b/beta"})
	if got["/a/alpha"] != "alpha" || got["/b/beta"] != "beta" {
		t.Errorf("expected plain basenames, got %v", got)
	}
}

func TestAssignNames_CollisionIsDeterministicAndDistinct(t *testing.T) {
	root := t.TempDir()
	// Sorted order decides who keeps the plain name, independent of input order.
	got := AssignNames(root, []string{"/z/samename", "/a/samename"})
	if got["/a/samename"] != "samename" {
		t.Errorf("lexicographically first path should own the plain dir, got %v", got)
	}
	second := got["/z/samename"]
	if second == "samename" {
		t.Fatalf("second project did not get a distinct dir: %v", got)
	}
	if !strings.HasPrefix(second, "samename-") {
		t.Errorf("collision dir %q should be the basename plus a suffix", second)
	}

	// Reproducible: a fresh call (no markers on disk yet) yields the same map.
	again := AssignNames(t.TempDir(), []string{"/z/samename", "/a/samename"})
	if again["/z/samename"] != second {
		t.Errorf("collision name is not reproducible: %q vs %q", again["/z/samename"], second)
	}
}

func TestAssignNames_MarkerKeepsOwnership(t *testing.T) {
	root := t.TempDir()
	// The second-sorted path already rendered into the plain directory.
	plain := filepath.Join(root, "samename")
	if err := WriteMarker(plain, "/z/samename"); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	got := AssignNames(root, []string{"/a/samename", "/z/samename"})
	if got["/z/samename"] != "samename" {
		t.Errorf("marker owner lost its directory: %v", got)
	}
	if got["/a/samename"] == "samename" {
		t.Errorf("new project took an already owned directory: %v", got)
	}
	// A third project must not land on either of them.
	third := AssignNames(root, []string{"/a/samename", "/z/samename", "/m/samename"})
	seen := map[string]bool{}
	for _, name := range third {
		if seen[name] {
			t.Fatalf("two projects got the same wiki dir: %v", third)
		}
		seen[name] = true
	}
}

func TestAssignNames_SkipsEmptyNames(t *testing.T) {
	got := AssignNames(t.TempDir(), []string{"/a/x", "", "/", "..", "."})
	if len(got) != 1 || got["/a/x"] != "x" {
		t.Errorf("expected only the usable project, got %v", got)
	}
}

// Cold-review F1: the loser of a collision used to move to a new directory on
// the second tick, because its own marker was read as "taken by someone else"
// and the chosen name was not registered for the rest of the batch.
func TestAssignNames_CollisionDirSurvivesRepeatedTicks(t *testing.T) {
	root := t.TempDir()
	projects := []string{"/a/samename", "/m/samename", "/z/samename"}

	first := AssignNames(root, projects)
	for _, p := range projects {
		if err := WriteMarker(filepath.Join(root, first[p]), p); err != nil {
			t.Fatalf("write marker for %s: %v", p, err)
		}
	}

	for tick := 2; tick <= 3; tick++ {
		got := AssignNames(root, projects)
		for _, p := range projects {
			if got[p] != first[p] {
				t.Errorf("tick %d: %s moved from %q to %q", tick, p, first[p], got[p])
			}
		}
	}
}

// Cold-review F5: two project keys that clean to the same path must not share
// a directory.
func TestAssignNames_NoDuplicateDirs(t *testing.T) {
	root := t.TempDir()
	if err := WriteMarker(filepath.Join(root, "x"), "/other/x"); err != nil {
		t.Fatal(err)
	}

	got := AssignNames(root, []string{"/a/x", "/a/x/"})
	if got["/a/x"] == got["/a/x/"] {
		t.Fatalf("both keys got %q", got["/a/x"])
	}
}

func TestWriteMarker_RoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "yesmem")
	if MarkerProject(dir) != "" {
		t.Fatalf("expected no marker before writing")
	}
	if err := WriteMarker(dir, "/home/tester/memory/yesmem"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if got := MarkerProject(dir); got != "/home/tester/memory/yesmem" {
		t.Errorf("MarkerProject = %q", got)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Errorf("WriteMarker should create the directory: %v", err)
	}
}

// Security review: a project path can end in a directory name with trailing
// whitespace. The marker has to survive that round-trip, otherwise the project's
// directory moves on every tick until the probes run out and the project is
// dropped.
func TestWriteMarker_PreservesWhitespaceInIdentifier(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "foo")
	project := "/a/foo "
	if err := WriteMarker(dir, project); err != nil {
		t.Fatal(err)
	}
	if got := MarkerProject(dir); got != project {
		t.Fatalf("marker round-trip changed the identifier: %q → %q", project, got)
	}

	root := t.TempDir()
	projects := []string{"/a/foo ", "/z/foo"}
	first := AssignNames(root, projects)
	for _, p := range projects {
		if err := WriteMarker(filepath.Join(root, first[p]), p); err != nil {
			t.Fatal(err)
		}
	}
	for tick := 2; tick <= 3; tick++ {
		got := AssignNames(root, projects)
		for _, p := range projects {
			if got[p] != first[p] {
				t.Errorf("tick %d: %q moved from %q to %q", tick, p, first[p], got[p])
			}
		}
	}
}

// Cold-review F2: a reader that knows the data dir must land on the directory
// the writer rendered into, collision or not.
func TestDir_ResolvesCollisionThroughMarkers(t *testing.T) {
	root := t.TempDir()
	projects := []string{"/a/samename", "/z/samename"}
	names := AssignNames(root, projects)
	for _, p := range projects {
		if err := WriteMarker(filepath.Join(root, names[p]), p); err != nil {
			t.Fatal(err)
		}
	}

	for _, p := range projects {
		if got, want := Dir(root, p), filepath.Join(root, names[p]); got != want {
			t.Errorf("Dir(%s) = %q, want %q", p, got, want)
		}
	}
	if Dir(root, "/a/samename") == Dir(root, "/z/samename") {
		t.Errorf("both projects resolve to the same directory")
	}
}

func TestDir_UnrenderedProject(t *testing.T) {
	root := t.TempDir()

	if got, want := Dir(root, "/a/x"), filepath.Join(root, "x"); got != want {
		t.Errorf("Dir on an empty root = %q, want %q", got, want)
	}

	// Plain directory claimed by another project: the reader reports the
	// collision name the writer will use.
	if err := WriteMarker(filepath.Join(root, "x"), "/other/x"); err != nil {
		t.Fatal(err)
	}
	got := Dir(root, "/a/x")
	if got == filepath.Join(root, "x") {
		t.Errorf("Dir pointed at a directory owned by another project")
	}
	if !strings.HasPrefix(filepath.Base(got), "x-") {
		t.Errorf("Dir = %q, want the collision name", got)
	}
}

// Cold-review F3: a relative project name must hash to the same suffix no
// matter which directory the process runs in.
func TestCollisionName_IndependentOfWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	projects := []string{"/var/www/x/greenWebsite", "greenWebsite"}
	names := AssignNames(root, projects)

	// Re-running from a different working directory must not move anything.
	other := t.TempDir()
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(root) })
	again := AssignNames(root, projects)

	if again["greenWebsite"] != names["greenWebsite"] {
		t.Errorf("relative project name is not stable across working directories: %q vs %q",
			again["greenWebsite"], names["greenWebsite"])
	}
}

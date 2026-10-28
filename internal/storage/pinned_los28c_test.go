package storage

import (
	"testing"
	"time"
)

// YM-280 (COG-C24) regression tests. A session pin belongs to the session that
// created it: a /clear of a *foreign* session must not remove it, wherever it was
// bound. And a pin whose project names a directory the briefing cannot reach must
// still be found through the project the directory belongs to.

const (
	los28cProject = "/home/testuser/projects/myapp"
	los28cOther   = "/home/testuser/projects/other"
)

// A project-bound session pin of another session in the same project must
// survive that other session's /clear. runtime.db is shared by every session, and
// parallel sessions in one project are the normal case (worktree swarms).
func TestClearSessionPins_KeepsProjectPinOfAnotherSession(t *testing.T) {
	s := mustOpenFiles(t)
	seedPin(t, s, "session", los28cProject, "sess-a", "pin of sess-a", time.Now())
	seedPin(t, s, "session", "", "sess-b", "own project-less pin", time.Now())

	if err := s.ClearSessionPins(los28cProject, "sess-b"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got := pinContents(t, s, "session", los28cProject)
	if !pinHasContent(got, "pin of sess-a") {
		t.Errorf("foreign session's project pin was deleted by /clear; survived: %v", got)
	}
	if pinHasContent(got, "own project-less pin") {
		t.Errorf("own project-less pin must be cleared, survived: %v", got)
	}
}

// The ending session's own project-bound pin still dies with its /clear.
func TestClearSessionPins_RemovesOwnProjectPin(t *testing.T) {
	s := mustOpenFiles(t)
	seedPin(t, s, "session", los28cProject, "sess-a", "other session's pin", time.Now())
	seedPin(t, s, "session", los28cProject, "sess-b", "own project pin", time.Now())

	if err := s.ClearSessionPins(los28cProject, "sess-b"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got := pinContents(t, s, "session", los28cProject)
	if pinHasContent(got, "own project pin") {
		t.Errorf("own project pin must be cleared, survived: %v", got)
	}
	if !pinHasContent(got, "other session's pin") {
		t.Errorf("other session's pin must survive, got: %v", got)
	}
}

// A pin is visible in the briefing of the project that owns the directory it was
// written in, and it must disappear with its session even though its project
// string never equals the project key.
func TestClearSessionPins_RemovesOwnPinBoundToSubdirectory(t *testing.T) {
	s := mustOpenFiles(t)
	seedSession(t, s, "sess-b", los28cProject)
	seedPin(t, s, "session", los28cProject+"/internal", "sess-b", "own subdir pin", time.Now())
	seedPin(t, s, "session", los28cProject+"/internal", "sess-a", "foreign subdir pin", time.Now())

	if err := s.ClearSessionPins(los28cProject, "sess-b"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got := pinContents(t, s, "session", los28cProject)
	if pinHasContent(got, "own subdir pin") {
		t.Errorf("own pin bound to a subdirectory survived /clear: %v", got)
	}
	if !pinHasContent(got, "foreign subdir pin") {
		t.Errorf("foreign session's pin must survive, got: %v", got)
	}
}

// Pins that predate session attribution carry no session_id. They cannot be
// attributed, so they keep dying with their project instead of becoming immortal.
func TestClearSessionPins_RemovesUnattributedProjectPin(t *testing.T) {
	s := mustOpenFiles(t)
	seedPin(t, s, "session", los28cProject, "", "legacy project pin", time.Now())
	seedPin(t, s, "session", "", "", "legacy project-less pin", time.Now())

	if err := s.ClearSessionPins(los28cProject, "sess-b"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got := pinContents(t, s, "session", los28cProject)
	if pinHasContent(got, "legacy project pin") {
		t.Errorf("unattributed project pin must die with its project, survived: %v", got)
	}
	if !pinHasContent(got, "legacy project-less pin") {
		t.Errorf("project-less unattributed pin must not die with a foreign project, got: %v", got)
	}
}

// A pin stays visible when its stored project names a directory that belongs to
// the requested project, and only then. String prefixes are not enough: they
// showed a pin of a vendored clone or of a sibling checkout in an unrelated
// project.
func TestGetPinnedLearnings_VisibleThroughOwningProject(t *testing.T) {
	s := mustOpenFiles(t)
	seedSession(t, s, "sess-app", los28cProject)
	seedSession(t, s, "sess-other", los28cOther)

	now := time.Now()
	seedPin(t, s, "permanent", los28cProject, "", "exact project pin", now)
	seedPin(t, s, "permanent", los28cProject+"/internal", "", "subdirectory pin", now)
	seedPin(t, s, "permanent", "myapp", "", "legacy short-name pin", now)
	seedPin(t, s, "permanent", "", "", "global pin", now)
	seedPin(t, s, "permanent", los28cOther+"/vendor/deep", "", "vendored clone pin", now)
	seedPin(t, s, "permanent", "/home/testuser/projects", "", "parent directory pin", now)
	seedPin(t, s, "permanent", los28cOther, "", "sibling project pin", now)

	got := pinContents(t, s, "permanent", los28cProject)
	for _, want := range []string{"exact project pin", "subdirectory pin", "legacy short-name pin", "global pin"} {
		if !pinHasContent(got, want) {
			t.Errorf("%q not visible in %s, got: %v", want, los28cProject, got)
		}
	}
	for _, unwanted := range []string{"vendored clone pin", "parent directory pin", "sibling project pin"} {
		if pinHasContent(got, unwanted) {
			t.Errorf("%q must not be visible in %s, got: %v", unwanted, los28cProject, got)
		}
	}

	other := pinContents(t, s, "permanent", los28cOther)
	if pinHasContent(other, "legacy short-name pin") {
		t.Errorf("a legacy short name must not leak into another project, got: %v", other)
	}
	if !pinHasContent(other, "vendored clone pin") {
		t.Errorf("a pin of a subdirectory must belong to the owning project, got: %v", other)
	}

	// The rules reminder asks with the raw cwd of the request, so a subdirectory
	// must see the pins of the project that owns it.
	fromSubdir := pinContents(t, s, "permanent", los28cProject+"/internal")
	if !pinHasContent(fromSubdir, "exact project pin") {
		t.Errorf("project pin not visible from a subdirectory, got: %v", fromSubdir)
	}
	if pinHasContent(fromSubdir, "sibling project pin") {
		t.Errorf("sibling project pin leaked into a subdirectory view, got: %v", fromSubdir)
	}
}

// A pin must stay visible to the directory that wrote it even after the indexer
// gives that directory its own session row: visibility and ownership resolve with
// the same function at the same moment, so nothing moves out from under the pin.
func TestGetPinnedLearnings_StaysVisibleWhenTheDirectoryGainsASession(t *testing.T) {
	s := mustOpenFiles(t)
	subdir := los28cProject + "/internal"
	seedSession(t, s, "sess-app", los28cProject)
	seedPin(t, s, "permanent", subdir, "", "subdir pin", time.Now())

	if !pinHasContent(pinContents(t, s, "permanent", los28cProject), "subdir pin") {
		t.Fatal("pin not visible in the owning project before the subdirectory has a session")
	}

	seedSession(t, s, "sess-sub", subdir)
	if !pinHasContent(pinContents(t, s, "permanent", subdir), "subdir pin") {
		t.Error("pin became invisible to its own directory after that directory became a project")
	}
}

// OwningProject is the identity the briefing and the pin lookups share: the
// directory itself when a session carries it, else the nearest ancestor (at most
// three levels), else the path as given.
func TestOwningProject_WalksUpToTheProjectCarryingTheSession(t *testing.T) {
	s := mustOpenFiles(t)
	seedSession(t, s, "sess-app", los28cProject)

	cases := []struct{ dir, want string }{
		{los28cProject, los28cProject},
		{los28cProject + "/internal", los28cProject},
		{los28cProject + "/internal/daemon", los28cProject},
		{los28cProject + "/a/b/c/d", los28cProject + "/a/b/c/d"},
		{"/no/sessions/anywhere", "/no/sessions/anywhere"},
		{"", ""},
	}
	for _, c := range cases {
		if got := s.OwningProject(c.dir); got != c.want {
			t.Errorf("OwningProject(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

// A legacy short name that two projects carry is ambiguous, so the pin stays
// hidden instead of appearing in both. Fail-closed: showing it twice is what the
// earlier resolution against the caller's view did wrong.
func TestGetPinnedLearnings_AmbiguousShortNameStaysHidden(t *testing.T) {
	s := mustOpenFiles(t)
	seedSession(t, s, "sess-app", los28cProject)
	seedSession(t, s, "sess-second", "/home/other/myapp")
	seedPin(t, s, "permanent", "myapp", "", "ambiguous pin", time.Now())

	if pinHasContent(pinContents(t, s, "permanent", los28cProject), "ambiguous pin") {
		t.Error("pin with an ambiguous legacy short name must stay hidden")
	}
}

// The visibility lookup runs per distinct pin project after the row scan. On a
// store whose handles share one connection (:memory:, read-only) a query inside
// the open cursor deadlocks, which is invisible until a pin names a foreign
// project.
func TestGetPinnedLearnings_DoesNotBlockOnSharedConnection(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.PinLearning("permanent", "", "global pin", "test", ""); err != nil {
		t.Fatalf("pin global: %v", err)
	}
	if _, err := s.PinLearning("permanent", los28cProject, "project pin", "test", ""); err != nil {
		t.Fatalf("pin project: %v", err)
	}

	pins, err := s.GetPinnedLearnings("permanent", "/home/testuser/projects")
	if err != nil {
		t.Fatalf("get pins: %v", err)
	}
	if len(pins) != 1 || pins[0].Content != "global pin" {
		t.Errorf("want only the global pin for a foreign project view, got %v", pins)
	}
}

// The legacy resolution must not treat the stored name as a pattern and must not
// fold case: a pin named '%' or 'MyApp' belongs nowhere it does not name exactly.
func TestGetPinnedLearnings_LegacyShortNameMustMatchExactly(t *testing.T) {
	s := mustOpenFiles(t)
	seedSession(t, s, "sess-app", los28cProject)
	now := time.Now()
	seedPin(t, s, "permanent", "%", "", "wildcard pin", now)
	seedPin(t, s, "permanent", "_yapp", "", "underscore pin", now)
	seedPin(t, s, "permanent", "MyApp", "", "case-variant pin", now)
	seedPin(t, s, "permanent", "unknown", "", "unknown short name pin", now)

	got := pinContents(t, s, "permanent", los28cProject)
	for _, unwanted := range []string{"wildcard pin", "underscore pin", "case-variant pin", "unknown short name pin"} {
		if pinHasContent(got, unwanted) {
			t.Errorf("%q must stay hidden for %s, got: %v", unwanted, los28cProject, got)
		}
	}
}

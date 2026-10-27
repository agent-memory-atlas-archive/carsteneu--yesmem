package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// A broadcast for a project without sessions used to be stored under whatever
// short name the caller passed, so the row landed in a project nobody reads and
// stayed orphaned once the project's first session named its real path. The
// caller's own directory is that project when it carries the name.
func TestLos57eStrictUnknownShortNameUsesTheCwdThatCarriesIt(t *testing.T) {
	s := mustOpen(t)
	cwd := "/home/user/newrepo"

	got, err := s.ResolveProjectShortStrict("newrepo", cwd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != cwd {
		t.Fatalf("ResolveProjectShortStrict(%q, %q) = %q, want the cwd path", "newrepo", cwd, got)
	}
}

// A caller that stands in some other directory gets no answer: the name resolves
// to nothing, and a basename handed back made callers persist it. A caller with no
// directory at all has nothing to compare against and keeps the name (dispatch
// marks that value unresolved, so nothing is written under it).
func TestLos57eStrictUnknownShortNameWithoutACwdIsUnresolved(t *testing.T) {
	s := mustOpen(t)

	got, err := s.ResolveProjectShortStrict("unknown-project", "/home/user/somewhere-else")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("ResolveProjectShortStrict(%q, other cwd) = %q, want \"\"", "unknown-project", got)
	}

	got, err = s.ResolveProjectShortStrict("unknown-project", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "unknown-project" {
		t.Errorf("ResolveProjectShortStrict(%q, no cwd) = %q, want the name back unchanged", "unknown-project", got)
	}
}

// A relative cwd is not a project location: taken as one, it would mint a scope
// out of caller bytes (security review, YM-300).
func TestLos57eStrictRefusesARelativeCwd(t *testing.T) {
	s := mustOpen(t)

	got, err := s.ResolveProjectShortStrict("newrepo", "rel/newrepo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("ResolveProjectShortStrict(%q, %q) = %q, want \"\"", "newrepo", "rel/newrepo", got)
	}
}

// A relative path with separators is reduced to its last element, so the basename
// match cannot widen to a suffix of a stored path.
func TestLos57eResolveProjectShortReducesARelativePathToItsBasename(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "los57e-rel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	base := time.Now()
	insertSessionForResolve(t, s, "s1", "/srv/a/b", "b", base)
	insertSessionForResolve(t, s, "s2", "/other/srv/a/b", "deep", base.Add(time.Minute))

	if got := s.ResolveProjectShort("srv/a/b"); got != "b" {
		t.Fatalf("ResolveProjectShort(%q) = %q, want only the last-element hit", "srv/a/b", got)
	}
}

// A short name must match the last path element only. The substring match routed
// "yesmem" into every checkout whose path happened to contain the name.
func TestLos57eResolveProjectShortMatchesTheBasenameOnly(t *testing.T) {
	// File-backed: the substring branch this test judges used to query under its
	// own cursor, which on a single-connection store hides the match it makes.
	s, err := Open(filepath.Join(t.TempDir(), "los57e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	base := time.Now()
	// Only this checkout carries "yesmem" as its last path element.
	insertSessionForResolve(t, s, "s1", "/srv/a/yesmem", "yesmem-wt", base)
	// This one merely contains the name, and it is the more recent session, so the
	// substring match would prefer it.
	insertSessionForResolve(t, s, "s2", "/srv/yesmem-old", "yesmem-old", base.Add(time.Minute))

	if got := s.ResolveProjectShort("yesmem"); got != "yesmem-wt" {
		t.Fatalf("ResolveProjectShort(%q) = %q, want only the basename hit", "yesmem", got)
	}
	// "yes" is a prefix of the basename, not the basename: no row may match.
	if got := s.ResolveProjectShort("yes"); got != "yes" {
		t.Fatalf("ResolveProjectShort(%q) = %q, want the name back unchanged", "yes", got)
	}
}

// The legacy short-name branch queried the database while its own cursor was
// open. On a single-connection store (":memory:") the inner query waits for the
// connection the cursor holds, and the call never returns.
func TestLos57eResolveProjectShortDoesNotQueryUnderItsOwnCursor(t *testing.T) {
	s := mustOpen(t)
	// project_short differs from the path's basename, so the exact fast path
	// misses and the basename match has to walk a cursor.
	insertSessionForResolve(t, s, "s1", "/srv/a/yesmem", "yesmem-wt", time.Now())

	done := make(chan string, 1)
	go func() { done <- s.ResolveProjectShort("yesmem") }()

	select {
	case got := <-done:
		if got != "yesmem-wt" {
			t.Fatalf("ResolveProjectShort(%q) = %q, want the resolved short name", "yesmem", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ResolveProjectShort blocked on a single-connection store: it queries while its cursor is open")
	}
}

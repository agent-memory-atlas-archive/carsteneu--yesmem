package storage

import (
	"testing"
)

// YM-286: a project id IS its absolute path since v0.65, so a path needs no
// lookup. Falling through to "" for a project that simply has no session row yet
// is what made resolveAgentWorkDir invent $HOME/projects/<basename>.
func TestLos32dResolveProjectPathKeepsAnAbsolutePath(t *testing.T) {
	s := mustOpen(t)
	const project = "/srv/los32d/projects/yesmem"

	if got := s.ResolveProjectPath(project); got != project {
		t.Fatalf("ResolveProjectPath(%q) = %q, want the path itself", project, got)
	}
}

// YM-286: the old query compared project_short against the bare name, and no row
// holds a bare name any more. A legacy short name has to resolve through the
// basename match against sessions.project.
func TestLos32dResolveProjectPathResolvesALegacyShortName(t *testing.T) {
	s := mustOpen(t)
	const project = "/srv/los32d/memory/yesmem"
	seedSession(t, s, "ses-los32d-short", project)

	if got := s.ResolveProjectPath("yesmem"); got != project {
		t.Fatalf("ResolveProjectPath(\"yesmem\") = %q, want %q", got, project)
	}
}

// Two checkouts share the name. Picking one of them is how a broadcast or a
// spawned agent lands in the wrong repository; the resolver has to refuse.
func TestLos32dResolveProjectPathRefusesAnAmbiguousShortName(t *testing.T) {
	s := mustOpen(t)
	seedSession(t, s, "ses-los32d-amb-a", "/srv/los32d/a/yesmem")
	seedSession(t, s, "ses-los32d-amb-b", "/srv/los32d/b/yesmem")

	if got := s.ResolveProjectPath("yesmem"); got != "" {
		t.Fatalf("ResolveProjectPath(\"yesmem\") = %q, want \"\" for an ambiguous name", got)
	}
}

// An unknown short name is not a path. Returning "" leaves the fallback decision
// with the caller instead of inventing a location.
func TestLos32dResolveProjectPathReturnsEmptyForAnUnknownShortName(t *testing.T) {
	s := mustOpen(t)

	if got := s.ResolveProjectPath("los32d-nowhere"); got != "" {
		t.Fatalf("ResolveProjectPath(unknown) = %q, want \"\"", got)
	}
}

// A short name is matched literally. "_" and "%" are LIKE wildcards, so an
// unescaped match resolved a name that is not the project's at all.
func TestLos32dResolveProjectPathKeepsShortNamesLiteral(t *testing.T) {
	s := mustOpen(t)
	seedSession(t, s, "ses-los32d-like", "/srv/los32d/x/proXect")

	if got := s.ResolveProjectPath("pro_ect"); got != "" {
		t.Fatalf("ResolveProjectPath(\"pro_ect\") = %q, want \"\": the name is not the project's", got)
	}
}

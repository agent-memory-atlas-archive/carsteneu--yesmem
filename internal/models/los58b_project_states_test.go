package models

import "testing"

// YM-424: an unassigned project has no basename. filepath.Base("") is ".", which
// turned "no project" into a project named ".".
func TestLos58bCanonicalProjectOfAnEmptyProjectIsEmpty(t *testing.T) {
	if got := CanonicalProject(""); got != "" {
		t.Fatalf("CanonicalProject(\"\") = %q, want \"\" (an unassigned row names no project)", got)
	}
}

// YM-424: the cluster key is an identity. A bare name carries no path, so it must
// not mint a key of its own: that key space is exactly what collided with the
// basenames of unrelated repositories.
func TestLos58bClusterProjectKeyMintsNoBasenameKey(t *testing.T) {
	if got := ClusterProjectKey("yesmem"); got != "" {
		t.Errorf("ClusterProjectKey(%q) = %q, want \"\": a bare name names no repository", "yesmem", got)
	}
	if got := ClusterProjectKey(""); got != "" {
		t.Errorf("ClusterProjectKey(\"\") = %q, want \"\"", got)
	}
	if got := ClusterProjectKey("/"); got != "" {
		t.Errorf("ClusterProjectKey(%q) = %q, want \"\": the filesystem root is no project", "/", got)
	}

	const repo = "/home/tester/memory/yesmem"
	if got := ClusterProjectKey(repo + "/.worktrees/yesloop-los-58b"); got != repo {
		t.Errorf("a worktree must fold onto its repository root, got %q", got)
	}
	// Two repositories that share a basename keep two keys.
	if ClusterProjectKey("/home/tester/memory/yesmem") == ClusterProjectKey("/home/tester/projects/yesmem") {
		t.Error("two repositories with the same basename share one cluster key")
	}
}

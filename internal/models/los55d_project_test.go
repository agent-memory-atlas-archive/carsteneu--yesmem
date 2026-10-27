package models

import (
	"os"
	"path/filepath"
	"testing"
)

// los55dRepo creates the given directory with a .git entry, so the root detection
// has something real to find. t.TempDir keeps the test independent of the checkout
// it runs in.
func los55dRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("create repo %s: %v", dir, err)
	}
	return dir
}

// los55dDir creates a plain directory without any .git below or above it.
func los55dDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create dir %s: %v", dir, err)
	}
	return dir
}

// YM-321 (2): the root was the path up to the worktree marker and nothing else, so
// a directory inside a repository counted as a repository of its own (one cluster
// key per subdirectory) and a nested checkout could not be told apart. The root is
// the nearest directory that carries a .git entry.
func TestLos55d_RepositoryRootWalksUpToTheCheckout(t *testing.T) {
	base := t.TempDir()
	repo := los55dRepo(t, filepath.Join(base, "repo"))
	sub := los55dDir(t, filepath.Join(repo, "internal", "daemon"))
	nested := los55dRepo(t, filepath.Join(repo, "vendor", "other"))

	cases := []struct {
		name    string
		project string
		want    string
	}{
		{"repo root", repo, repo},
		{"subdirectory", sub, repo},
		{"trailing slash on a subdirectory", sub + "/", repo},
		{"nested checkout", nested, nested},
		{"inside the nested checkout", filepath.Join(nested, "pkg"), nested},
	}
	for _, tc := range cases {
		if got := RepositoryRoot(tc.project); got != tc.want {
			t.Errorf("%s: RepositoryRoot(%q) = %q, want %q", tc.name, tc.project, got, tc.want)
		}
	}
}

// A worktree keeps resolving to the checkout that hosts it, and a directory that
// belongs to no checkout at all keeps its own path: the walk-up must not invent a
// root where there is none.
func TestLos55d_RepositoryRootKeepsWorktreesAndRootlessPaths(t *testing.T) {
	repo := los55dRepo(t, filepath.Join(t.TempDir(), "repo"))
	worktree := los55dDir(t, filepath.Join(repo, ".worktrees", "yesloop-los-55d"))
	orphan := los55dDir(t, filepath.Join(t.TempDir(), "loose", "dir"))

	cases := []struct {
		name    string
		project string
		want    string
	}{
		{"worktree", worktree, repo},
		{"worktree subdirectory", filepath.Join(worktree, "internal"), repo},
		{"no checkout above", orphan, orphan},
		{"bare name", "repo", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := RepositoryRoot(tc.project); got != tc.want {
			t.Errorf("%s: RepositoryRoot(%q) = %q, want %q", tc.name, tc.project, got, tc.want)
		}
	}
}

// The cluster key must pair with exactly one learnings scope. A directory inside a
// checkout has its own basename as scope, so it keeps its own key: sharing the
// checkout's key would let one project's clustering run replace the other's rows.
func TestLos55d_ClusterProjectKeyKeepsSubdirectoryScopesApart(t *testing.T) {
	base := t.TempDir()
	repo := los55dRepo(t, filepath.Join(base, "repo"))
	sub := los55dDir(t, filepath.Join(repo, "plays"))
	worktree := los55dDir(t, filepath.Join(repo, ".worktrees", "yesloop-los-55d"))

	cases := []struct {
		name    string
		project string
		want    string
	}{
		{"checkout root", repo, repo},
		{"subdirectory project keeps its own key", sub, sub},
		{"worktree folds onto the checkout", worktree, repo},
		{"worktree subdirectory", filepath.Join(worktree, "internal"), repo},
		// YM-424: a bare name names no repository, so it mints no key.
		{"bare name", "repo", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := ClusterProjectKey(tc.project); got != tc.want {
			t.Errorf("%s: ClusterProjectKey(%q) = %q, want %q", tc.name, tc.project, got, tc.want)
		}
	}

	// The invariant the cluster writer relies on: the key carries the basename of
	// the project it belongs to.
	if CanonicalProject(sub) == CanonicalProject(repo) {
		t.Fatalf("the test needs two differing scopes, got %q twice", CanonicalProject(repo))
	}
	if filepath.Base(ClusterProjectKey(sub)) != CanonicalProject(sub) {
		t.Errorf("key %q does not carry the scope %q", ClusterProjectKey(sub), CanonicalProject(sub))
	}
}

// The root of one path is resolved over and over in the request path, so the answer
// is cached per path. A stale cache entry would be worse than the stat it saves, so
// the cached value must match a cold call.
func TestLos55d_RepositoryRootIsStableAcrossCalls(t *testing.T) {
	repo := los55dRepo(t, filepath.Join(t.TempDir(), "repo"))
	sub := los55dDir(t, filepath.Join(repo, "deep", "deeper"))

	first := RepositoryRoot(sub)
	second := RepositoryRoot(sub)
	if first != repo || second != repo {
		t.Fatalf("RepositoryRoot(%q) = %q then %q, want %q twice", sub, first, second, repo)
	}
}

// A checkout at the home directory is a config repository, not the project of every
// plain directory below it. Without the stop one dotfiles repository would collapse
// unrelated projects onto a single cluster key and mix their cluster sets.
func TestLos55d_RepositoryRootStopsAtTheHomeCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	los55dRepo(t, home)

	plain := los55dDir(t, filepath.Join(home, "projects", "plain"))
	if got := RepositoryRoot(plain); got != plain {
		t.Errorf("RepositoryRoot(%q) = %q, want the path itself (the home checkout must not claim it)", plain, got)
	}

	repo := los55dRepo(t, filepath.Join(home, "projects", "repo"))
	if got := RepositoryRoot(filepath.Join(repo, "internal")); got != repo {
		t.Errorf("RepositoryRoot(%q) = %q, want %q", filepath.Join(repo, "internal"), got, repo)
	}
}

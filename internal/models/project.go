package models

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProjectMatches reports whether two project identifiers refer to the same project.
// Since project identifiers are now full absolute paths (see ProjectShortFromPath),
// a simple equality check is both necessary and sufficient. Earlier fuzzy matching
// (suffix/basename) caused collisions between distinct repos sharing a folder name.
func ProjectMatches(a, b string) bool {
	return a == b
}

// worktreeMarkers are the directory segments that put a path inside a worktree
// checkout of a repository. Both layouts are in use: yesmem spawns under
// ".worktrees/", other tooling nests under ".claude/worktrees/".
var worktreeMarkers = []string{"/.worktrees/", "/.claude/worktrees/"}

// ProjectOrigin says how a project identifier came into being. The three states are
// kept apart on purpose: an empty value names no project, a bare name names one it
// cannot be resolved to, and a path names one exactly. Collapsing them into a
// basename-shaped string made an unknown project indistinguishable from a global row
// and let a bare name pass as the identity of any repository with the same basename
// (YM-424).
type ProjectOrigin int

const (
	// ProjectOriginUnassigned: the row names no project at all (a global learning, an
	// empty session row).
	ProjectOriginUnassigned ProjectOrigin = iota
	// ProjectOriginPath: an absolute path, possibly inside a worktree checkout.
	ProjectOriginPath
	// ProjectOriginLegacy: a bare name from before paths were stored. It names a
	// project, but no path follows from it, so consumers treat it as unknown.
	ProjectOriginLegacy
)

// ClassifyProject returns the state a project identifier is in.
func ClassifyProject(project string) ProjectOrigin {
	switch {
	case project == "":
		return ProjectOriginUnassigned
	case filepath.IsAbs(project):
		return ProjectOriginPath
	default:
		return ProjectOriginLegacy
	}
}

// CanonicalProject returns the canonical (parent) project basename for a CWD path.
// If CWD is inside a worktree directory, returns the parent directory basename.
// Otherwise returns filepath.Base(cwd).
//
// The value is a grouping marker, NOT an identity: two repositories that share a
// basename share it. Call ClassifyProject when the question is which project a value
// names. An empty value returns "": there is no basename of no path (filepath.Base("")
// is ".").
func CanonicalProject(cwd string) string {
	if cwd == "" {
		return ""
	}
	cut := len(cwd)
	for _, marker := range worktreeMarkers {
		if i := strings.Index(cwd, marker); i >= 0 && i < cut {
			cut = i
		}
	}
	return filepath.Base(cwd[:cut])
}

// repositoryRootTTL bounds how long a positive root answer is trusted. A checkout does
// not lose its .git entry, but a new one can appear between an input path and the root it
// resolved to: a nested clone created after the fact (a vendor checkout inside a
// repository) would otherwise keep resolving to the enclosing repository for the
// lifetime of the process (YM-390).
const repositoryRootTTL = 30 * time.Second

// repositoryRootEntry is one cached answer plus the moment it was computed. Without the
// moment, a positive entry could only be dropped by a restart.
type repositoryRootEntry struct {
	root      string
	checkedAt time.Time
}

// repositoryRootNow is the clock the cache entries are aged against; a test moves it to
// prove that an answer older than the window is recomputed.
var repositoryRootNow = time.Now

// repositoryRootCache memoises resolved roots per input path. Only positive answers
// are cached: a checkout does not lose its .git entry, while a directory that has
// none today may be cloned into tomorrow, and a cached empty answer would keep that
// clone invisible for the lifetime of the process.
var repositoryRootCache sync.Map // input path -> repositoryRootEntry

// stripWorktreeMarkers returns project without a worktree suffix, or "" when the
// value names no path.
func stripWorktreeMarkers(project string) string {
	if project == "" || !filepath.IsAbs(project) {
		return ""
	}
	cut := len(project)
	for _, marker := range worktreeMarkers {
		if i := strings.Index(project, marker); i >= 0 && i < cut {
			cut = i
		}
	}
	root := filepath.Clean(project[:cut])
	if root == string(filepath.Separator) {
		return ""
	}
	return root
}

// RepositoryRoot returns the root path of the repository that contains project: the
// nearest directory at or above the worktree-stripped path that carries a .git
// entry, else that path itself. Stripping the worktree marker alone was not enough:
// a directory inside a checkout counted as a checkout of its own, and a nested
// checkout was only recognised when its own path was passed as a project. A value
// that is not an absolute path (a legacy short name) returns "", because it names
// no root.
//
// The ascent stops at the home directory. A checkout there (a dotfiles repository)
// is a config repository, not the project of every plain directory below it; without
// the stop one such repository would collapse unrelated projects onto one identity.
//
// ClusterProjectKey deliberately does not use this answer: see there.
func RepositoryRoot(project string) string {
	stripped := stripWorktreeMarkers(project)
	if stripped == "" {
		return ""
	}
	if cached, ok := repositoryRootCache.Load(project); ok {
		if entry, ok := cached.(repositoryRootEntry); ok && repositoryRootNow().Sub(entry.checkedAt) < repositoryRootTTL {
			return entry.root
		}
		repositoryRootCache.Delete(project)
	}
	root := stripped
	if found := enclosingCheckout(root); found != "" {
		root = found
		repositoryRootCache.Store(project, repositoryRootEntry{root: root, checkedAt: repositoryRootNow()})
	}
	return root
}

// enclosingCheckout returns the nearest directory at or above path that carries a
// .git entry, or "" when the path belongs to no checkout. The home directory and
// everything above it is out of reach, and a symlinked project path keeps its own
// identity: resolving links would merge checkouts that the caller deliberately
// addresses by two paths.
func enclosingCheckout(path string) string {
	home, _ := os.UserHomeDir()
	for dir := path; ; {
		if home != "" && dir == home {
			return ""
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ClusterProjectKey returns the key a project's cluster rows live under: the
// project's own path, with every worktree of a repository folded onto that
// repository. A repository is identified by its path, so two repositories that share
// a basename keep their own cluster rows.
//
// A bare name (a legacy project identifier) and an empty value return "" and mint no
// key: neither names a repository, and a basename is not an identity (YM-424). The
// caller decides what an unknown project means; the writer refuses the empty key.
//
// The key deliberately stops at the project path and does NOT follow the containing
// checkout (RepositoryRoot). One key carries exactly one learnings scope, the
// canonical basename (models.CanonicalProject, the value stored on every learning
// row), and the clustering writer replaces every row of the key it writes. Two
// project paths inside one checkout have different basenames ("repo" and
// "repo/plays"), so sharing the checkout's key would let one project's run replace
// the other's rows and make them flip-flop with the activity order. Narrowing the
// learnings scope instead would need a data migration across all learnings.
func ClusterProjectKey(project string) string {
	if ClassifyProject(project) != ProjectOriginPath {
		return ""
	}
	return stripWorktreeMarkers(project)
}

// DisplayName returns a short human-readable label for a project path, consisting
// of the last two path segments (e.g. "ccm19/cookie-consent-management"). Used for
// briefing headers and list_projects output where the full path would be too verbose.
func DisplayName(path string) string {
	if path == "" {
		return ""
	}
	cleaned := filepath.Clean(path)
	parent := filepath.Dir(cleaned)
	base := filepath.Base(cleaned)
	parentBase := filepath.Base(parent)
	if parentBase == "/" || parentBase == "." || parentBase == base {
		return base
	}
	return parentBase + "/" + base
}

// DisplayNameOfShort returns a display label from any project identifier, accepting
// both full paths and legacy short names. Short names are returned unchanged.
func DisplayNameOfShort(short string) string {
	if short == "" {
		return ""
	}
	if short[0] == '/' {
		return DisplayName(short)
	}
	return short
}

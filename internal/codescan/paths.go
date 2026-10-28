package codescan

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// ResolveScanRoot returns the canonical form of rootDir: absolute, cleaned and
// symlink-free as far as the path exists. filepath.Walk lstats its root, so a
// root that is itself a symlink arrives as a plain file entry and the tree
// below it is never entered (YM-232).
//
// A tail that does not exist yet is kept as given, but the longest existing
// prefix is resolved: otherwise the same project reached through a link and
// through its real path would produce two different keys.
func ResolveScanRoot(rootDir string) string {
	if rootDir == "" {
		return ""
	}
	dir := rootDir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, ok := resolveExistingPrefix(dir); ok {
		return resolved
	}
	return filepath.Clean(dir)
}

// ResolveWithinRoot resolves rel against rootDir and reports whether the result
// stays inside it. rel is client-supplied or walk-relative, so filepath.Join
// alone is not enough: Join resolves ".." and returns a path outside rootDir
// without complaint (YM-199 / DEV-DT35).
//
// Symlinks are resolved on the deepest existing prefix, so a link inside the
// project that points out of it is rejected as well. A link whose target does
// not exist cannot be checked either, and a later read would follow it once the
// target appears, so such a path is rejected as well (fail-closed). A path
// outside the project is a legitimate read only if the caller widened rootDir
// itself.
//
// Walk-side callers pass the walk-relative path, never the raw walk path:
// filepath.Walk returns paths that already carry rootDir, and joining those
// onto the root a second time leaves a path that does not exist, which hides
// the symlink from the check (Learning #102156).
func ResolveWithinRoot(rootDir, rel string) (string, bool) {
	if rootDir == "" || rel == "" {
		return "", false
	}
	root, err := filepath.Abs(rootDir)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	target := rel
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target, ok := resolveExistingPrefix(filepath.Clean(target))
	if !ok {
		return "", false
	}

	within, err := filepath.Rel(root, target)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", false
	}
	return target, true
}

// resolveExistingPrefix resolves symlinks along the longest existing prefix of
// path and keeps the tail as given, so a path that does not exist yet is still
// checked against the project it is meant to land in.
//
// The second result is false when the path itself is a symlink that cannot be
// resolved: its target does not exist (yet), so the check cannot see where a
// later read would land. Accepting it would leave the containment test blind
// exactly for the path the read follows (YM-232).
func resolveExistingPrefix(path string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, true
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	dir, base := filepath.Split(path)
	if base == "" {
		// No component left to check: the cleanup in the callers makes this a
		// programming error, and returning the path unchanged would pass an
		// unresolved prefix through as safe.
		return "", false
	}
	dir = filepath.Clean(dir)
	if dir == path || dir == string(filepath.Separator) {
		return path, true
	}
	resolvedDir, ok := resolveExistingPrefix(dir)
	if !ok {
		return "", false
	}
	return filepath.Join(resolvedDir, base), true
}

// logSkippedLinks reports one line per walk, not per link: the path names are
// repository content, and a repository with many links could flood the log.
// Only symlinked source files are counted — a directory symlink never reaches
// this check, because filepath.Walk does not descend into one.
func logSkippedLinks(scope, rootDir string, links int, first string) {
	if links == 0 {
		return
	}
	log.Printf("[codescan] %s: skipped %d symlinked source file(s) that resolve outside %q or not at all (first: %q)", scope, links, rootDir, first)
}

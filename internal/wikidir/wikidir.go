// Package wikidir is the single source of truth for where a project's code
// wiki lives on disk.
//
// Layout: <root>/<short>/, where <root> is <dataDir>/wiki and <short> is the
// project's basename ("/home/tester/memory/yesmem" → "yesmem"). The writer
// (wiki-tick, `yesmem wiki-render`) and every reader (code-map header,
// [yesmem-wiki-first] directive, docs) resolve through this package. When they
// disagreed, renders landed in wiki/home/tester/memory/yesmem/ while every
// reader kept pointing at a stale wiki/yesmem/ (YM-6 / DEV-DT28).
//
// Collisions: two live projects can share a basename (three checkouts named
// "cookie-consent-management", #80175). The plain basename belongs to the
// project recorded in <dir>/.project; every other project with that basename
// gets "<base>-<8 hex of the cleaned path>". Within one run AssignNames
// processes projects in sorted order, so the assignment is deterministic, and
// the markers make it stable across runs. Readers that know the data dir use
// Dir, which reads the same markers and therefore finds the directory the
// writer rendered into.
package wikidir

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// RootName is the wiki directory inside the yesmem data dir.
	RootName = "wiki"
	// MarkerFile names the file recording which project owns a wiki directory.
	MarkerFile = ".project"
	// DisplayRoot is the wiki root as it appears in prompts and documentation.
	DisplayRoot = "~/.claude/yesmem/wiki"

	// maxCollisionProbes bounds the search for a free directory name when even
	// the collision name is taken.
	maxCollisionProbes = 20
)

// Root returns the wiki root inside a yesmem data dir.
func Root(dataDir string) string { return filepath.Join(dataDir, RootName) }

// ShortName returns the wiki directory name for a project identifier: the
// basename for a path, the identifier itself for an already short name.
func ShortName(project string) string {
	if project == "" {
		return ""
	}
	cleaned := filepath.Clean(project)
	if cleaned == "." || cleaned == ".." || cleaned == string(filepath.Separator) {
		return ""
	}
	if !strings.ContainsRune(cleaned, filepath.Separator) {
		return cleaned
	}
	return filepath.Base(cleaned)
}

// Dir returns the wiki directory for a project below root, using the .project
// markers to reproduce the writer's collision resolution, so a reader without
// knowledge of the project set lands on the directory the writer actually
// rendered into.
func Dir(root, project string) string {
	return filepath.Join(root, resolveName(root, project))
}

// DisplayDir returns the wiki directory for a project as it appears in docs and
// prompts when no data dir is known. Name-based: without a root it cannot
// consult the markers, so for a collision it names the plain directory (use Dir
// when the data dir is available).
func DisplayDir(project string) string {
	return DisplayRoot + "/" + ShortName(project)
}

// resolveName returns the directory name project owns below root: the marker
// match when the directory was rendered at least once, else the name the writer
// would pick first.
func resolveName(root, project string) string {
	base := ShortName(project)
	if base == "" || base == project {
		// Already a short name: no path the markers could be matched against.
		return base
	}
	// The plain directory is either free or this project's own in every case
	// except a collision — and a free one is what the writer picks first.
	if owner := MarkerProject(filepath.Join(root, base)); owner == "" || owner == project {
		return base
	}
	// Plain directory belongs to another project: find the one this project
	// claimed, else report the name the writer will pick for it.
	for _, name := range assignmentCandidates(project)[1:] {
		if MarkerProject(filepath.Join(root, name)) == project {
			return name
		}
	}
	return collisionName(project)
}

// AssignNames maps every project to the wiki directory name it owns below root.
// Projects are processed in sorted order: the first path of a colliding pair
// keeps the plain basename, the others get their collision name. A directory
// already owned by a different project (marker) is never handed over, so a
// rendered wiki keeps its place across ticks.
func AssignNames(root string, projects []string) map[string]string {
	sorted := make([]string, 0, len(projects))
	for _, p := range projects {
		if ShortName(p) != "" {
			sorted = append(sorted, p)
		}
	}
	sort.Strings(sorted)

	names := make(map[string]string, len(sorted))
	taken := make(map[string]string, len(sorted)) // directory name → project
	for _, p := range sorted {
		for _, name := range assignmentCandidates(p) {
			if !usableName(root, taken, name, p) {
				continue
			}
			names[p] = name
			taken[name] = p
			break
		}
	}
	return names
}

// assignmentCandidates returns the directory names a project may own, in
// priority order: the plain basename, then the collision name, then indexed
// variants of it.
func assignmentCandidates(project string) []string {
	base := ShortName(project)
	if base == "" {
		return nil
	}
	out := []string{base}
	collision := collisionName(project)
	if collision == base {
		return out
	}
	out = append(out, collision)
	for i := 2; i <= maxCollisionProbes; i++ {
		out = append(out, fmt.Sprintf("%s-%d", collision, i))
	}
	return out
}

// usableName reports whether name below root is unclaimed or already owned by
// project itself (so a project keeps the directory it rendered into).
func usableName(root string, taken map[string]string, name, project string) bool {
	if owner, ok := taken[name]; ok && owner != project {
		return false
	}
	owner := MarkerProject(filepath.Join(root, name))
	return owner == "" || owner == project
}

// collisionName is the deterministic fallback name for project: its short name
// plus a hash of the cleaned path. The path is used as given (not resolved
// against the process working directory), so a relative project name hashes to
// the same suffix no matter where the process started.
func collisionName(project string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(project)))
	return ShortName(project) + "-" + hex.EncodeToString(sum[:4])
}

// WriteMarker records project as the owner of the wiki directory dir, creating
// the directory if needed. The identifier is stored byte for byte: a project
// path may end in a character that trimming would eat (a directory named
// "foo "), and a marker that reads back differently would make the project
// migrate to a new directory on every tick.
func WriteMarker(dir, project string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MarkerFile), []byte(project), 0644)
}

// MarkerProject returns the project recorded as owner of the wiki directory
// dir, or "" when no marker exists yet. Only a trailing line break is stripped,
// so hand-edited markers still work.
func MarkerProject(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, MarkerFile))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
}

package indexer

import (
	"path/filepath"
	"strings"
)

// normalizeExcludedProject renders one project reference in the form the
// exclusion lookup uses: trimmed, separator-cleaned and lowercased. The list is
// written by hand in config.yaml, so a trailing separator, a doubled separator
// or a stray space must not turn an entry into a dead one. An empty result
// means "no usable entry" and never matches.
func normalizeExcludedProject(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if p == "." || p == string(filepath.Separator) {
		return ""
	}
	return strings.ToLower(p)
}

// normalizeExcludedProjects builds the lookup map both indexers share. One
// function, not one per constructor: the two used to lowercase on their own, and
// a rule added to one of them would have silently missed the other (YM-320).
func normalizeExcludedProjects(list []string) map[string]bool {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]bool, len(list))
	for _, p := range list {
		if key := normalizeExcludedProject(p); key != "" {
			out[key] = true
		}
	}
	return out
}

// projectExcluded reports whether a session belongs to a project on the
// exclusion list. Both operands go through the same normalization: the list is
// configured by hand, while project and project_short come from stored rows and
// parsed files, and either side may carry a trailing separator or an upper case
// spelling. An empty name never matches.
func projectExcluded(exclude map[string]bool, project, short string) bool {
	if len(exclude) == 0 {
		return false
	}
	if key := normalizeExcludedProject(project); key != "" && exclude[key] {
		return true
	}
	key := normalizeExcludedProject(short)
	return key != "" && exclude[key]
}

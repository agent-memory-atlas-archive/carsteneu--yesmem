package codescan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/carsteneu/yesmem/internal/wikidir"
)

// renderWikiLink builds the code-map header for a project. project is the
// project identifier as the caller knows it — a repository path (normal case)
// or a short name; both resolve to the same wiki directory as the writer uses.
func renderWikiLink(project string) string {
	// DisplayRoot is home-relative; resolve it against HOME instead of
	// hardcoding the wiki location a second time.
	rel := strings.TrimPrefix(wikidir.DisplayRoot, "~/")
	name := wikidir.ShortName(project)
	hasName := name != ""
	if !hasName {
		name = "project"
	}
	timestamp := "not yet rendered — will appear after first wiki-tick"
	if home, err := os.UserHomeDir(); err == nil {
		root := filepath.Join(home, rel)
		if hasName {
			// Marker-aware: a basename shared with another live project renders
			// into a suffixed directory (see wikidir).
			name = filepath.Base(wikidir.Dir(root, project))
		}
		if info, err := os.Stat(filepath.Join(root, name, "health.md")); err == nil {
			timestamp = fmt.Sprintf("last build: %s", info.ModTime().Format("15:04"))
		}
	}
	wikiPath := "~/" + filepath.Join(rel, name) + "/"
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n**Full code map:** `%s` (%s)\n\n", wikiPath, timestamp))
	b.WriteString("> BEFORE editing any file, check its wiki page for per-file\n")
	b.WriteString("> learnings, gotchas, and co-edit context: `files/<path>.md`\n")
	b.WriteString("> (symbols, imports, related sessions). Path encoding: `/` → `_`.\n")
	b.WriteString("> Package overview: `packages.md` (file counts, gotchas, TODOs).\n")
	b.WriteString("> Full file tree: `index.md`.\n")
	b.WriteString("> Falls back to `search_code_index` / `get_code_snippet` / raw grep.\n\n")
	return b.String()
}

package hooks

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/carsteneu/yesmem/internal/codescan"
)

var navTools = map[string]bool{
	"grep": true, "egrep": true, "fgrep": true, "rg": true,
	"sed": true, "awk": true,
	"cat": true, "head": true, "tail": true,
	"find": true,
}

var excludedPrefixes = []string{
	"/var/log/", "/var/cache/", "/tmp/", "/etc/",
	"/proc/", "/sys/", "/dev/",
}

var excludedExtensions = []string{".log", ".txt", ".md", ".gitignore", ".gitattributes", ".gitmodules", ".gitconfig"}

// maxNavTargets bounds how many index lookups one shell line may trigger. A
// command is model-written, and the opencode plugin applies the same bound.
var maxNavTargets = 32

// shellBoundary reports whether a byte ends a token in a shell line. A quote only
// opens at such a byte, so an apostrophe inside a word ("don't") stays a literal.
func shellBoundary(c byte) bool {
	switch c {
	case ' ', '\t', '\n', ';', '&', '|', '<', '>', '(', ')':
		return true
	}
	return false
}

// stripRedirectGlue removes a file-descriptor number glued to a redirect operator
// (the "2" in `2>/dev/null`) and a trailing background "&". Neither is an argument:
// the digit would otherwise become a navigation target, which changed the command's
// identity and made the advertised retry look like a new command (YM-409).
func stripRedirectGlue(segment string) string {
	s := strings.TrimRight(segment, " \t")
	if s == "" {
		return s
	}
	if s[len(s)-1] == '&' {
		s = strings.TrimRight(s[:len(s)-1], " \t")
	}
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s
	}
	if i == 0 {
		return ""
	}
	switch s[i-1] {
	case ' ', '\t', ';', '&', '|':
		return strings.TrimRight(s[:i], " \t")
	}
	return s
}

// shellSegments splits a shell line at its operators, so a navigation command
// behind `cd x &&` or after a `;` is still seen (YM-409; the same rule the opencode
// plugin applies in splitCommandSegments, YM-84). An operator inside a quote is a
// literal, and a backslash defuses the next byte.
func shellSegments(cmd string) []string {
	var segments []string
	var cur strings.Builder
	var quote byte

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segments = append(segments, s)
		}
		cur.Reset()
	}

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\\' && i+1 < len(cmd) {
			cur.WriteByte(c)
			cur.WriteByte(cmd[i+1])
			i++
			continue
		}
		if (c == '"' || c == '\'' || c == '`') && (i == 0 || shellBoundary(cmd[i-1])) {
			quote = c
			cur.WriteByte(c)
			continue
		}
		if c == '&' && i+1 < len(cmd) && cmd[i+1] == '&' {
			flush()
			i++
			continue
		}
		if c == '\n' || c == ';' || c == '|' {
			flush()
			continue
		}
		if c == '<' || c == '>' {
			if s := stripRedirectGlue(cur.String()); s != "" {
				segments = append(segments, s)
			}
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return segments
}

// navCandidate reports whether an argument can name an indexed path. Not a path:
// a glob (the shell expands it), an unexpanded substitution (its value is not
// known here), a shell separator, and a bare number (the operand of a flag such as
// -c 100). Everything else is asked, because the store is the only place that knows
// the file tree; a narrow extension list here would exempt whole languages
// (YM-409 review). The opencode plugin applies the same rule.
func navCandidate(arg string) bool {
	if arg == "" {
		return false
	}
	if strings.ContainsAny(arg, "*?[{}$`&;|") {
		return false
	}
	digits := true
	for _, r := range arg {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	return !digits
}

// parseNavSegment returns the navigation tool of one shell segment and the file
// arguments it names. ok is false for a segment that reads no file.
func parseNavSegment(segment string) (tool string, files []string, ok bool) {
	tokens := tokenizeShell(segment)
	if len(tokens) == 0 {
		return "", nil, false
	}

	base := filepath.Base(tokens[0])
	if !navTools[base] {
		return "", nil, false
	}
	tool = base

	var positional []string
	for i := 1; i < len(tokens); i++ {
		t := tokens[i]
		if strings.HasPrefix(t, "-") {
			continue
		}
		positional = append(positional, t)
	}

	switch tool {
	case "grep", "egrep", "fgrep", "rg", "sed", "awk":
		// The first positional argument is the pattern, not a file.
		if len(positional) < 2 {
			return "", nil, false
		}
		files = positional[1:]
	case "cat", "head", "tail", "find":
		files = positional
	}

	var valid []string
	for _, f := range files {
		if !navCandidate(f) || isExcludedPath(f) {
			continue
		}
		valid = append(valid, f)
	}
	if len(valid) == 0 {
		return "", nil, false
	}
	return tool, valid, true
}

// NavTarget is one file a shell line navigates, with the tool that reads it.
type NavTarget struct {
	Tool string
	File string
}

// ParseNavTargets resolves every navigation target of a shell line, following
// cd/pushd/popd as the opencode plugin does (YM-84). A file argument is made
// absolute against the directory its segment runs in, so the lookup checks the
// file the command really names (YM-409). With an empty cwd the arguments stay as
// written.
func ParseNavTargets(cmd, cwd string) []NavTarget {
	var targets []NavTarget
	dir := cwd
	var stack []string

	for _, segment := range shellSegments(cmd) {
		tokens := tokenizeShell(segment)
		if len(tokens) == 0 {
			continue
		}
		switch name := filepath.Base(tokens[0]); name {
		case "cd", "pushd", "popd":
			if name == "popd" {
				if n := len(stack); n > 0 {
					dir = stack[n-1]
					stack = stack[:n-1]
				}
				continue
			}
			dest := ""
			for _, t := range tokens[1:] {
				if !strings.HasPrefix(t, "-") {
					dest = t
					break
				}
			}
			if dest == "" {
				continue
			}
			if name == "pushd" {
				stack = append(stack, dir)
			}
			dir = absoluteFrom(dir, dest)
			continue
		}

		tool, files, ok := parseNavSegment(segment)
		if !ok {
			continue
		}
		for _, f := range files {
			targets = append(targets, NavTarget{Tool: tool, File: absoluteFrom(dir, f)})
			// One hook call may not trigger an unbounded number of index lookups: a
			// command is model-written, and the plugin applies the same bound.
			if len(targets) >= maxNavTargets {
				return targets
			}
		}
	}
	return targets
}

// absoluteFrom resolves a file argument against dir. An empty dir keeps the
// argument as written.
func absoluteFrom(dir, file string) string {
	if dir == "" {
		return file
	}
	if filepath.IsAbs(file) {
		return filepath.Clean(file)
	}
	return filepath.Clean(filepath.Join(dir, file))
}

// ParseNavCommand checks if a shell command is a code-navigation command
// targeting specific files. Returns the tool name, file paths, and whether
// it matches. The files stay as written; ParseNavTargets resolves them against a
// directory and follows cd/pushd/popd.
func ParseNavCommand(cmd string) (tool string, files []string, ok bool) {
	for _, segment := range shellSegments(cmd) {
		if t, f, found := parseNavSegment(segment); found {
			return t, f, true
		}
	}
	return "", nil, false
}

func tokenizeShell(cmd string) []string {
	var tokens []string
	var cur strings.Builder
	var quote byte

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\\' && i+1 < len(cmd):
			// A backslash escapes the next byte: `my\ dir` is one token.
			i++
			cur.WriteByte(cmd[i])
		case (c == '"' || c == '\'' || c == '`') && (i == 0 || shellBoundary(cmd[i-1])):
			quote = c
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func isExcludedPath(path string) bool {
	for _, prefix := range excludedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	ext := filepath.Ext(path)
	for _, exc := range excludedExtensions {
		if ext == exc {
			return true
		}
	}
	return false
}

var (
	reShDouble = regexp.MustCompile(`sh\(\s*"([^"]+)"`)
	reShSingle = regexp.MustCompile(`sh\(\s*'([^']+)'`)
	reShBack   = regexp.MustCompile("sh\\(\\s*`([^`]+)`")
	reRgCall   = regexp.MustCompile(`(?:^|[^a-zA-Z_])rg\(\s*["']([^"']+)["']\s*,\s*["']([^"']+)["']`)
	reCatCall  = regexp.MustCompile(`(?:^|[^a-zA-Z_])cat\(\s*["']([^"']+)["']`)
)

func ParseREPLNavCommands(code string) []string {
	var cmds []string
	for _, re := range []*regexp.Regexp{reShDouble, reShSingle, reShBack} {
		for _, m := range re.FindAllStringSubmatch(code, -1) {
			inner := m[1]
			_, _, ok := ParseNavCommand(inner)
			if ok {
				cmds = append(cmds, inner)
			}
		}
	}
	for _, m := range reRgCall.FindAllStringSubmatch(code, -1) {
		cmds = append(cmds, fmt.Sprintf(`rg "%s" %s`, m[1], m[2]))
	}
	for _, m := range reCatCall.FindAllStringSubmatch(code, -1) {
		cmds = append(cmds, fmt.Sprintf("cat %s", m[1]))
	}
	return cmds
}

// SuggestYesmemTool returns a concrete yesmem MCP tool call to use
// instead of the given shell navigation tool on the given file. The values are
// sanitized: the advice is a quoted pseudo-call, and a repository can hold a file
// whose name closes that quote.
func SuggestYesmemTool(navTool, filePath, project string) string {
	filePath = messageSafe(filePath)
	project = messageSafe(project)
	switch navTool {
	case "grep", "egrep", "fgrep", "rg":
		return fmt.Sprintf("search_code_index(\"%s\", \"%s\") for symbol search, or search_code(\"<symbol>(\", \"%s\") for call sites and text",
			filepath.Base(filePath), project, project)
	case "sed":
		return fmt.Sprintf("get_code_snippet(file=\"%s\", project=\"%s\", start_line=x, end_line=y)", filePath, project)
	case "cat":
		return fmt.Sprintf("get_file_symbols(\"%s\", \"%s\") for overview, then get_code_snippet(file=\"%s\", project=\"%s\", start_line=x, end_line=y)",
			filePath, project, filePath, project)
	case "head", "tail":
		return fmt.Sprintf("get_code_snippet(file=\"%s\", project=\"%s\", start_line=x, end_line=y)", filePath, project)
	case "awk", "find":
		return fmt.Sprintf("get_code_snippet(file=\"%s\", project=\"%s\", start_line=x, end_line=y) or search_code(pattern, \"%s\")",
			filePath, project, project)
	}
	return ""
}

// messageSafe keeps a value from breaking the quoted pseudo-call in the block or
// hint message: control characters, line separators, invisible characters, bidi
// overrides, quotes and backticks are stripped. The opencode plugin's safe() strips
// a smaller subset, so this answer is the stricter of the two surfaces.
func messageSafe(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '"' || r == '`':
			return -1
		case r < 0x20 || r == 0x7f:
			return -1
		case r >= 0x80 && r <= 0x9f:
			return -1
		case r == 0x2028 || r == 0x2029:
			return -1
		case r >= 0x200b && r <= 0x200f:
			return -1
		case r >= 0x202a && r <= 0x202e:
			return -1
		case r == 0x00ad || r == 0x061c || r == 0xfeff:
			// soft hyphen, arabic letter mark, zero-width no-break space
			return -1
		case r >= 0x2060 && r <= 0x206f:
			// word joiner and invisible operators, bidi isolates, deprecated formats
			return -1
		}
		return r
	}, value)
}

// codeNavRoot is the directory the index lookup is anchored at: the checkout that
// encloses cwd. A navigation out of a subdirectory (`cat ../daemon/x.go`) is a
// project file, and the opencode plugin, which works from its project directory,
// blocks it (YM-431). The index rows themselves are written relative to the
// directory the scan walked, which is not always this root, so the lookup asks
// both spellings. A cwd that belongs to no checkout keeps itself as its root.
//
// The ascent is codescan.CheckoutRoot and not a copy of it: the index and the hook
// answer the same question, and two implementations of one rule drift (YM-436). The
// value is deliberately not models.RepositoryRoot, which folds the worktrees of one
// repository onto the main root for its learnings scope, while the index keeps one
// key per checkout and this anchor is the checkout the command runs in
// (TestLos59eCodeNavRoot).
func codeNavRoot(cwd string) string {
	// A relative path names no checkout to ascend from: the ascent would resolve it
	// against this process's working directory, not against the command's.
	if cwd == "" || !filepath.IsAbs(cwd) {
		return cwd
	}
	return codescan.CheckoutRoot(cwd)
}

// escapesProjectRoot reports whether the path lies outside the checkout the index
// is rooted at. The index only holds paths inside it, so such a target cannot
// match, and it must not make the command look like a navigation of a project file
// (the plugin skips these targets as well).
func escapesProjectRoot(root, file string) bool {
	if root == "" || !filepath.IsAbs(file) {
		return false
	}
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CheckCodeNav is the entry point for code-navigation detection in the
// PreToolUse hook. Returns a block reason and whether to block the command.
// cwd is the directory the shell line runs in (relative operands resolve against
// it), while the index questions are asked against the enclosing checkout.
// The isIndexed callback checks whether a path is in the codescan index, for a
// file as well as for a directory operand (recursive grep, find).
func CheckCodeNav(cmd, cwd, project, sessionID string, isIndexed func(string, string) bool, dismissed bool) (reason string, block bool) {
	if dismissed {
		return "", false
	}
	target, hit := firstIndexedTarget(cmd, cwd, project, isIndexed)
	if !hit {
		return "", false
	}
	safeFile := messageSafe(target.File)
	safeProject := messageSafe(project)
	reason = fmt.Sprintf("BLOCKED: Use yesmem MCP tools instead of shell navigation for indexed file %s\n"+
		"  → %s\n"+
		"  Call sites of a function: search_code(\"<symbol>(\", \"%s\") returns them with file and line.",
		safeFile, SuggestYesmemTool(target.Tool, target.File, project), safeProject)
	if sessionID != "" {
		reason += fmt.Sprintf("\n  If you need this shell command anyway: dismiss_code_nav(session_id=\"%s\") lifts the block for this session.",
			messageSafe(sessionID))
	}
	return reason, true
}

// NavHint is the nudge counterpart of CheckCodeNav: the same detection, worded as
// advice. code_nav_mode "nudge" asks for a hint and not for a lock, so nothing here
// reports a block, and the dismissal escape is pointless because nothing holds the
// call back (YM-436).
func NavHint(cmd, cwd, project string, isIndexed func(string, string) bool) (hint string, ok bool) {
	target, hit := firstIndexedTarget(cmd, cwd, project, isIndexed)
	if !hit {
		return "", false
	}
	return fmt.Sprintf("HINT: yesmem has code-navigation tools for indexed file %s (code_nav_mode: nudge)\n"+
		"  → %s",
		messageSafe(target.File), SuggestYesmemTool(target.Tool, target.File, project)), true
}

// firstIndexedTarget returns the first navigation target of cmd that the index
// knows, in command order.
func firstIndexedTarget(cmd, cwd, project string, isIndexed func(string, string) bool) (NavTarget, bool) {
	root := codeNavRoot(cwd)
	for _, target := range ParseNavTargets(cmd, cwd) {
		if escapesProjectRoot(root, target.File) {
			continue
		}
		file := indexRelativePath(root, target.File)
		hit := isIndexed(project, file)
		if !hit {
			// A scan can be rooted below the checkout: the scanner walks the caller's
			// project directory while the rows are keyed under the checkout. For a
			// command that runs in or below such a directory, the spelling relative to
			// it is what the index holds, and it is the shape the store's lookup is
			// built for (internal/storage/codenav.go). Asking only the checkout-relative
			// spelling silently stopped matching a subdirectory-rooted index (YM-431
			// review). A target above cwd has no such spelling — indexRelativePath
			// returns it absolute — so that second question is not asked.
			if local := indexRelativePath(cwd, target.File); local != file && !filepath.IsAbs(local) {
				hit = isIndexed(project, local)
			}
		}
		if hit {
			return target, true
		}
	}

	return NavTarget{}, false
}

// indexRelativePath rewrites an absolute path into a path relative to the
// directory the index is rooted at. The code index stores paths relative to the
// project root, so a shell command naming an absolute file (cat /home/…/x.go)
// matched nothing before (DT31 / YM-7). Paths that cannot be expressed relative
// to it stay untouched.
func indexRelativePath(cwd, file string) string {
	if cwd == "" || !filepath.IsAbs(file) {
		return file
	}
	rel, err := filepath.Rel(cwd, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return file
	}
	return rel
}

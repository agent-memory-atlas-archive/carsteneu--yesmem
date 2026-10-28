package hooks

import (
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// shellNoise contains common shell tokens to skip during keyword extraction.
var shellNoise = map[string]bool{
	"sudo": true, "bash": true, "sh": true, "zsh": true,
	"set": true, "echo": true, "printf": true,
	"if": true, "then": true, "else": true, "fi": true,
	"do": true, "done": true, "for": true, "while": true,
	"case": true, "esac": true, "in": true,
	"export": true, "local": true, "readonly": true,
	"true": true, "false": true, "exit": true,
	"dev": true, "null": true, "tmp": true,
	"cat": true, "head": true, "tail": true, "grep": true,
	"and": true, "the": true, "not": true,
	"test": true, "internal": true, "count": true, "timeout": true, "run": true,
}

// expandHomeTokens rewrites the home shorthands "~/", "$HOME" and "${HOME}" to the
// home directory. A command's path words must not depend on how the caller spelled
// the home anchor: "rm -rf ~/.claude/yesmem" left only [".claude", "yesmem"] where
// the absolute form also carried the home and user segments, and the hard block on
// that command class needs a match score of blockMinScore — so the same command sat
// two points lower for one spelling than for the other (YM-310). The added segments
// are path noise and stay out of the keyword set except in the path-only fallback.
//
// Consequence, accepted: once a command consists of nothing but such a path, both
// spellings reach blockMinScore for a gotcha that names the path, whatever the verb
// is — "ls ~/.claude/yesmem" is blocked as well. The absolute form always did that;
// the fallback treats the path as the whole signal on purpose, and narrowing it to
// destructive verbs would weaken the block this fallback exists for.
func expandHomeTokens(cmd string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return cmd
	}
	home = strings.ToLower(home)

	var out strings.Builder
	out.Grow(len(cmd) + len(home))
	for i := 0; i < len(cmd); {
		rest := cmd[i:]
		switch {
		case strings.HasPrefix(rest, "~/"):
			out.WriteString(home)
			i++ // keep the separator for the next round
		case strings.HasPrefix(rest, "${home}"):
			out.WriteString(home)
			i += len("${home}")
		case strings.HasPrefix(rest, "$home") && (len(rest) == len("$home") || !isWordByte(rest[len("$home")])):
			out.WriteString(home)
			i += len("$home")
		default:
			out.WriteByte(cmd[i])
			i++
		}
	}
	return out.String()
}

// isWordByte reports whether b continues a variable name.
func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// extractKeywords extracts meaningful tokens from a bash command, a REPL code
// body, or a WebFetch host. It is also what builds the failure-search query in
// assist.go (buildSearchQuery), so a filtered token is missing there too.
func extractKeywords(cmd string) []string {
	lower := strings.ToLower(cmd)
	lower = expandHomeTokens(lower)

	// Replace non-alphanumeric chars with spaces (keep _ - .)
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			return r
		}
		return ' '
	}, lower)

	words := strings.Fields(normalized)
	seen := make(map[string]bool)
	userNames := homeUserNames(lower)
	var keywords, pathWords []string

	for _, w := range words {
		// Skip flags
		if strings.HasPrefix(w, "-") {
			continue
		}
		// Skip short words
		if len(w) < 3 {
			continue
		}
		// Skip shell noise
		if shellNoise[w] {
			continue
		}
		// Deduplicate
		if seen[w] {
			continue
		}
		seen[w] = true
		// Skip path noise: a command carries the paths it runs against, and a
		// segment that says nothing about which file is meant is no better as a
		// keyword here than it is in a file path. Without this filter a command
		// that merely runs inside a worktree of this checkout produced
		// ".worktrees", "yesmem" and "memory", each of which hasLongKeywordMatch
		// admits on its own (YM-269). The user-name segment of a home directory is
		// the same class, whatever it is called.
		if pathNoise[w] || userNames[w] {
			pathWords = append(pathWords, w)
			continue
		}
		keywords = append(keywords, w)
	}
	if len(keywords) == 0 {
		// Nothing but path noise is left, so the path IS the whole signal. Without
		// this fallback the command "rm -rf /home/<user>/.claude/yesmem" produced no
		// keyword at all ("rm" and "rf" fall below the length floor), RunCheck returned
		// before its gotcha lookup, and the hard block on the one command class that
		// destroys the installation was gone. Keeping the filter for every command
		// that has another token left is what the measurement above asks for; an empty
		// result has no discrimination left to protect.
		return pathWords
	}
	return keywords
}

// homeUserNames returns the user-name segment of every "/home/<name>" and
// "/users/<name>" in an already lowercased command. The list carries "chief"
// because this checkout lives under it, but a user name is path noise on every
// installation, and the destructive-command class must not depend on whose name the
// list happens to contain. Every "/home/<x>" path would otherwise leave exactly one
// short keyword ("alice", "bob") and drop below the match threshold.
func homeUserNames(cmd string) map[string]bool {
	var names map[string]bool
	for _, root := range []string{"/home/", "/users/"} {
		for from := 0; from < len(cmd); {
			rel := strings.Index(cmd[from:], root)
			if rel < 0 {
				break
			}
			start := from + rel + len(root)
			end := start
			for end < len(cmd) && cmd[end] != '/' && cmd[end] != ' ' && cmd[end] != '\t' {
				end++
			}
			if end > start {
				if names == nil {
					names = make(map[string]bool)
				}
				names[cmd[start:end]] = true
			}
			from = start
		}
	}
	return names
}

// matchScore counts how many keywords appear in the gotcha content.
func matchScore(keywords []string, content string) int {
	lower := strings.ToLower(content)
	score := 0
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			score++
		}
	}
	return score
}

// hasLongKeywordMatch returns true if any keyword >= 6 chars matches the content.
// This allows single-keyword matches for specific terms like "sandbox", "systemctl".
func hasLongKeywordMatch(keywords []string, content string) bool {
	lower := strings.ToLower(content)
	for _, kw := range keywords {
		if len(kw) >= 6 && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// isFileLikeKeyword reports whether a keyword names a FILE rather than a path
// segment. Either its dot sits after the first rune ("main.go", ".claude.json"), or
// it is exactly the file this call edits and hides behind a leading dot (".env",
// ".gitignore"): on a file op the target tells the two apart, which no lexical rule
// can. In the command path a short hidden name stays ambiguous with a hidden
// directory (.git, .venv) and is therefore not file-like there.
func isFileLikeKeyword(kw, fileBase string) bool {
	if fileBase != "" && strings.HasPrefix(kw, ".") && kw == strings.ToLower(fileBase) {
		return true
	}
	return strings.LastIndex(kw, ".") > 0
}

// hasFileKeywordMatch returns true if any keyword naming a FILE (not a hidden path
// segment) matches. The name carries a dot AFTER its first rune ("main.go",
// ".claude.json"), while a hidden directory carries the dot at position 0 and
// nothing after it (".git", ".venv", ".config", ".cache", ".worktrees", ".claude").
// The old rule admitted every dotted keyword, so one hidden segment was specific
// enough to inject on its own below that directory (YM-269). fileBase is the name of
// the file an Edit or Write targets; it is empty for other inputs.
func hasFileKeywordMatch(keywords []string, content, fileBase string) bool {
	lower := strings.ToLower(content)
	for _, kw := range keywords {
		if isFileLikeKeyword(kw, fileBase) && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// minNestedMatchLen is the shortest string allowed to match as part of a longer
// one. Counted in runes: two-character entities ("sh", "go") are the noise class
// this bound exists for.
const minNestedMatchLen = 3

// entityKeywordMatch reports whether a learning's entity and an extracted keyword
// name the same subject. A nested hit counts only when the nested string sits on
// token boundaries and is at least minNestedMatchLen runes long, so the entities
// "sh" and "go" no longer match inside "push" or "category" (YM-159). Boundaries
// are any non-alphanumeric rune, which keeps dotted, dashed and slashed names
// matchable ("internal/hooks/check.go" ↔ "hooks", "src/main.go" ↔ "src").
func entityKeywordMatch(entity, keyword string) bool {
	entity = strings.ToLower(entity)
	keyword = strings.ToLower(keyword)
	if entity == "" || keyword == "" {
		return false
	}
	return containsOnTokenBoundary(entity, keyword) || containsOnTokenBoundary(keyword, entity)
}

// containsOnTokenBoundary reports whether needle occurs in haystack delimited by
// non-alphanumeric runes (or the string ends) on both sides. An identical pair
// always counts: the length bound governs a nested side, not identity. That bound
// counts runes, not bytes, so a single non-ASCII letter does not pass as a
// three-character nested side. Both inputs are expected lowercased.
func containsOnTokenBoundary(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	if needle == haystack {
		return true
	}
	needleRunes := utf8.RuneCountInString(needle)
	if needleRunes < minNestedMatchLen || needleRunes > utf8.RuneCountInString(haystack) {
		return false
	}
	for from := 0; from+len(needle) <= len(haystack); {
		rel := strings.Index(haystack[from:], needle)
		if rel < 0 {
			return false
		}
		start := from + rel
		end := start + len(needle)
		if boundaryBefore(haystack, start) && boundaryAfter(haystack, end) {
			return true
		}
		from = start + 1
	}
	return false
}

// boundaryBefore reports whether the rune before off is not a letter or digit.
func boundaryBefore(s string, off int) bool {
	if off <= 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:off])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// boundaryAfter reports whether the rune starting at off is not a letter or digit.
func boundaryAfter(s string, off int) bool {
	if off >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[off:])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// hasNonFilenameKeywordMatch reports whether content mentions one of the
// keywords outside of an echoed path and outside the file name itself. For a
// file op every keyword comes from the target path, so this asks whether the
// gotcha says anything about that path beyond repeating it.
//
// The file name is the last path segment as the caller sees it (filename), not
// whatever happens to contain a ".": Makefile, Dockerfile and README are file
// names too, and counting them as ordinary path words let a Bash transcript that
// merely echoes the name pass as overlap (MED-1). extractPathKeywords emits a
// dotted name together with its extension-less stem ("notes.txt" → "notes.txt",
// "notes"), so the stem is excluded as well.
//
// A directory segment that the transcript mentions as a standalone word is kept:
// that is a statement about the location rather than an echo of the path. That
// also holds for a dotted segment: a transcript that says "module y@v1.2.3 not
// found" is talking about the version directory, so a gotcha carrying it can
// select a file op under that path. The previous derivation excluded every
// dotted keyword, which suppressed that overlap as a side effect.
//
// Corner case, accepted: when an extension-less file name is also one of the
// path's directory segments (…/mcp/mcp), excluding the name removes the only
// keyword that could have matched, so the gotcha cannot fire at all. That errs
// towards silence, which is the safe direction for this guard.
func hasNonFilenameKeywordMatch(keywords []string, content, filename string) bool {
	// keywords are lowercased by extractPathKeywords, so compare in lower case.
	filenameParts := make(map[string]bool)
	if filename != "" {
		filename = strings.ToLower(filename)
		filenameParts[filename] = true
		if i := strings.LastIndex(filename, "."); i > 0 {
			if stem := filename[:i]; len(stem) >= 3 {
				filenameParts[stem] = true
			}
		}
	}

	var prose strings.Builder
	for _, tok := range strings.Fields(content) {
		if strings.ContainsAny(tok, `/\`) {
			continue
		}
		prose.WriteString(tok)
		prose.WriteByte(' ')
	}
	lower := strings.ToLower(prose.String())

	for _, kw := range keywords {
		if filenameParts[kw] {
			continue
		}
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// pathNoise contains generic path segments to skip during file path keyword extraction.
// "memory" and "yesmem" are the checkout directory names this repository lives
// under. They appear in nearly every gotcha text, so as keywords they lifted
// every edit below them to the warning threshold on their own (YM-160).
//
// ".worktrees" and ".claude" are the same class for a different reason: one sits
// in every path of a linked worktree, the other in every path of the config
// directory. When both counted as filename-like (hasFileKeywordMatch admitted every
// dotted keyword), a gotcha that merely named them was injected on every edit below
// them (YM-226). They stay in the list although that rule is gone: dropping them
// from it would let them back in as ordinary match-score points, and as command
// keywords hasLongKeywordMatch admitted them on their length (YM-269).
//
// The list is shared by path and command keyword extraction. A segment that says
// nothing about WHICH file is meant is useless in both.
var pathNoise = map[string]bool{
	"home": true, "root": true, "usr": true, "var": true,
	"etc": true, "opt": true, "tmp": true, "dev": true,
	"src": true, "lib": true, "bin": true, "www": true,
	"html": true, "chief": true, "internal": true,
	"cmd": true, "pkg": true, "vendor": true,
	"memory": true, "yesmem": true,
	".worktrees": true, ".claude": true,
}

// dangerousActionVerbs lists the action words after which isRelevantForTool drops
// a gotcha for tools that cannot touch the working tree. The set is coarse on
// purpose: it is the list the substring test carried before, now matched as a
// whole token, so the comment above the helper owns the matching rule, not the
// word choice.
var dangerousActionVerbs = map[string]bool{
	"rm": true, "mv": true, "cp": true, "git": true, "make": true,
	"deploy": true, "chmod": true, "chown": true,
	"write": true, "edit": true, "delete": true, "remove": true,
}

// actionMentionsDangerousVerb reports whether the action text names one of
// dangerousActionVerbs as a whole token. Tokens are delimited by any rune that
// is neither a letter nor a digit, so "rm -rf", "git push" and "chmod 600 x"
// keep matching. A substring test read "rm" out of "response_format" and
// "terraform", and "cp" out of "mcp", which dropped unrelated gotchas for every
// non-file tool (YM-226).
//
// The whole-token rule errs towards admitting: an identifier or an inflected
// form ("scp", "os.WriteFile", "deleted") is not one of the verbs and counts as
// non-destructive. That can only add candidates for tools that cannot change
// the working tree, never remove one, and the match score still has to admit
// them.
func actionMentionsDangerousVerb(action string) bool {
	for _, tok := range strings.FieldsFunc(strings.ToLower(action), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if dangerousActionVerbs[tok] {
			return true
		}
	}
	return false
}

// extractPathKeywords extracts meaningful tokens from a file path.
// /home/user/project/internal/hooks/check.go
// → ["user", "project", "hooks", "check.go", "check"]
func extractPathKeywords(path string) []string {
	lower := strings.ToLower(path)
	parts := strings.Split(lower, "/")

	seen := make(map[string]bool)
	var keywords []string

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || len(p) < 3 {
			continue
		}
		if pathNoise[p] {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		keywords = append(keywords, p)

		// Also add filename without extension as separate keyword
		if strings.Contains(p, ".") {
			name := p[:strings.LastIndex(p, ".")]
			if len(name) >= 3 && !seen[name] && !pathNoise[name] {
				seen[name] = true
				keywords = append(keywords, name)
			}
		}
	}
	return keywords
}

// stripANSI removes ANSI escape sequences from text.
func stripANSI(s string) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		result.WriteByte(s[i])
		i++
	}
	return result.String()
}

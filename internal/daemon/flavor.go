package daemon

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// deriveSessionFlavor condenses a session narrative into the one-line flavor
// (max 80 chars) stored on the session's learnings. First sentence when it
// fits, otherwise a rune-safe, word-boundary-truncated prefix. The daemon
// narrative step calls this for every extracted session (Claude Code AND
// opencode) so the briefing composer always sees flavors — previously flavors
// existed only via the fork pipeline, which is config-disabled.
func deriveSessionFlavor(narrative string) string {
	narrative = strings.TrimSpace(narrative)
	if narrative == "" {
		return ""
	}
	flavor := narrative
	if end := firstSentenceEnd(narrative); end > 0 {
		flavor = narrative[:end]
	}
	if len(flavor) > 80 {
		flavor = truncateRunes(flavor, 80)
		if sp := strings.LastIndexByte(flavor, ' '); sp > 40 {
			flavor = flavor[:sp] // cut at spaces only — hyphens are part of German compounds
		}
	}
	return strings.TrimSpace(flavor)
}

// firstSentenceEnd returns the byte index after the first sentence-ending
// punctuation, or -1. A period followed by a lowercase letter or digit is an
// abbreviation ("usw.", "ca.", "3.5"); a period followed by a single
// uppercase letter and another period is one too ("z.B.", "u.a."), as is the
// period terminating that abbreviation tail ("z.B. DeepSeek").
func firstSentenceEnd(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.', '!', '?':
			if isSentenceEnd(s, i) {
				return i + 1
			}
		}
	}
	return -1
}

// isSentenceEnd reports whether the punctuation at s[i] truly ends a sentence.
func isSentenceEnd(s string, i int) bool {
	next := i + 1
	if next >= len(s) {
		return true
	}
	r, size := utf8.DecodeRuneInString(s[next:])
	if unicode.IsLower(r) || unicode.IsDigit(r) {
		return false // abbreviation ("usw.", "ca.") or decimal ("3.5")
	}
	if unicode.IsUpper(r) && next+size < len(s) && s[next+size] == '.' {
		return false // z.B. / u.a. pattern
	}
	// Period + space + uppercase: a real sentence end — unless the period
	// terminates a single-letter abbreviation tail ("z.B. DeepSeek").
	if r == ' ' && i >= 2 && s[i-2] == '.' {
		if prev, _ := utf8.DecodeLastRuneInString(s[:i]); unicode.IsUpper(prev) {
			return false
		}
	}
	return true
}

// truncateRunes cuts at or before n bytes without splitting a UTF-8 rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

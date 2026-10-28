package textutil

import (
	"strings"
	"unicode"
)

// defaultSingleLineRunes bounds a log line when the caller passes no limit.
const defaultSingleLineRunes = 300

// SingleLine flattens text for a log line: control characters and format
// characters become spaces, runs of whitespace collapse, and the result is
// capped at maxRunes. Text that can come from a model answer or a database error
// must pass through here before it is logged, otherwise a line break or a
// bidirectional override in it can fake or reorder a log entry.
func SingleLine(s string, maxRunes int) string {
	s = flatten(s)
	if maxRunes <= 0 {
		maxRunes = defaultSingleLineRunes
	}
	if runes := []rune(s); len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return s
}

// flatten collapses text for a log line without bounding it: control and format
// characters become spaces, runs of whitespace collapse.
func flatten(s string) string {
	s = strings.Map(func(r rune) rune {
		// C0, DEL, the C1 range (which carries the 8-bit CSI) and the Unicode
		// format characters, among them the bidirectional overrides.
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

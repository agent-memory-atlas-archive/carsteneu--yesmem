package textutil

import (
	"strings"
	"testing"
)

// Text that reaches a log line must not be able to forge a second entry.
func TestSingleLineFlattensControlCharacters(t *testing.T) {
	in := "boom\r\n2026/10/20 12:00:00   [1/1] ok deadbeef extraction ok\n\tende\x00\x1b"
	out := SingleLine(in, 300)
	if strings.ContainsAny(out, "\r\n\t\x00\x1b") {
		t.Fatalf("control characters survived: %q", out)
	}
	if !strings.HasPrefix(out, "boom 2026/10/20 12:00:00") {
		t.Fatalf("text was not flattened: %q", out)
	}
	if strings.Contains(out, "  ") {
		t.Fatalf("whitespace was not collapsed: %q", out)
	}
}

// The cap counts runes, so a UTF-8 sequence is never cut in half.
func TestSingleLineCapsRunes(t *testing.T) {
	out := SingleLine("äöüäöüäöü", 4)
	if out != "äöüä..." {
		t.Fatalf("SingleLine cap = %q, want %q", out, "äöüä...")
	}
}

// C1 controls and Unicode format characters can reorder or hide the rest of a
// log line, so they are flattened as well.
func TestSingleLineFlattensC1AndFormatCharacters(t *testing.T) {
	out := SingleLine("a\u009bb\u202ec\u200bd\ufeFFe", 300)
	for _, bad := range []string{"\u009b", "\u202e", "\u200b", "\uFEFF"} {
		if strings.Contains(out, bad) {
			t.Fatalf("character %q survived: %q", bad, out)
		}
	}
	if out != "a b c d e" {
		t.Fatalf("SingleLine = %q, want %q", out, "a b c d e")
	}
}

// No limit means the default limit, never an unbounded line.
func TestSingleLineUsesDefaultCapForNonPositiveLimit(t *testing.T) {
	out := SingleLine(strings.Repeat("a", defaultSingleLineRunes+50), 0)
	if len([]rune(out)) != defaultSingleLineRunes+3 {
		t.Fatalf("SingleLine length = %d runes, want %d", len([]rune(out)), defaultSingleLineRunes+3)
	}
}

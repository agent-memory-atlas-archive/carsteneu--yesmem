package codescan

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Foreign CLI stderr reaches daemon.log and the scan_cooldown row. Cutting it at
// a byte offset can split a multi-byte rune and put invalid UTF-8 into both
// (YM-311, escaped from YM-277). The euro sign is three bytes wide, so a
// 1024-byte cut lands inside a rune.
func TestStderrSuffix_StaysValidUTF8AtTheCut(t *testing.T) {
	long := strings.Repeat("€", 900) + " REASON-AT-THE-END"
	got := stderrSuffix(long)
	if !utf8.ValidString(got) {
		t.Fatalf("capped stderr is not valid UTF-8: %q", got)
	}
	if !strings.Contains(got, "REASON-AT-THE-END") {
		t.Errorf("truncation dropped the tail: %q", got)
	}
	if len(got) > maxStderrSuffix+len(":  ...(truncated)... ") {
		t.Errorf("capped stderr too long: %d bytes", len(got))
	}

	// The tail cut must land on a rune boundary as well.
	onlyRunes := strings.Repeat("€", 3000)
	if got := stderrSuffix(onlyRunes); !utf8.ValidString(got) {
		t.Fatalf("tail cut is not valid UTF-8: %q", got)
	}
}

// Control characters are not covered by collapsing whitespace: an ANSI escape or
// a bell in foreign output would recolor or rewrite the daemon log on the
// reader's terminal (YM-311).
func TestStderrSuffix_DropsControlCharacters(t *testing.T) {
	got := stderrSuffix("error: \x1b[31mproject not found\x1b[0m \x07")
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("control characters survived: %q", got)
	}
	if !strings.Contains(got, "project not found") {
		t.Fatalf("reason was dropped: %q", got)
	}

	// A bidi override is invisible itself and reverses the visible order of the
	// text after it, so it counts as a control character here.
	if got := stderrSuffix("a\u202Eb\u200Bc"); strings.ContainsAny(got, "\u202e\u200b") {
		t.Fatalf("format characters survived: %q", got)
	}

	// Invalid UTF-8 in the foreign text must leave the package as valid UTF-8.
	if got := stderrSuffix("bad \xff\xfe bytes"); !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 passed through: %q", got)
	}
}

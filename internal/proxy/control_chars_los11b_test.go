package proxy

import (
	"strings"
	"testing"
)

// los11bControlRune is the test oracle for "must not reach an injected line":
// C0 controls, DEL, C1 controls, and the Unicode line/paragraph separators.
// The implementation is free to share one helper for it, but the expectation
// lives here.
func los11bControlRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029
}

// YM-252 (PRX-P29): the loop warning flattens C0 controls and DEL in a path
// label (a55e54a0), but C1 controls and the Unicode line/paragraph separators
// survive and render as a line break inside the injected meta block.
func TestShortLoopPath_FlattensUnicodeLineSeparators_Los11b(t *testing.T) {
	got := shortLoopPath("/src/a\u2028b\u2029c\u0085d.go")
	if i := strings.IndexFunc(got, los11bControlRune); i >= 0 {
		t.Errorf("shortLoopPath kept a control rune at %d: %q", i, got)
	}
	if !strings.Contains(got, "a") || !strings.Contains(got, "d.go") {
		t.Errorf("shortLoopPath dropped the readable part: %q", got)
	}
}

// planNudgeName feeds a tool input path into an injected instruction; a newline
// or separator inside it forges a line of its own next to the meta blocks.
func TestPlanNudgeName_FlattensControlRunes_Los11b(t *testing.T) {
	cases := map[string]string{
		"newline":   "/x/pl\nan.md",
		"tab":       "/x/pl\tan.md",
		"C1":        "/x/pl\u0085an.md",
		"line sep":  "/x/pl\u2028an.md",
		"paragraph": "/x/pl\u2029an.md",
		"del":       "/x/pl\x7fan.md",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			got := planNudgeName(path)
			if i := strings.IndexFunc(got, los11bControlRune); i >= 0 {
				t.Fatalf("planNudgeName kept a control rune at %d: %q", i, got)
			}
			if strings.ContainsAny(got, "\n\r\t") {
				t.Fatalf("planNudgeName kept a line break: %q", got)
			}
			if got == "" || got == "plan file" {
				t.Fatalf("planNudgeName lost the name entirely: %q", got)
			}
		})
	}
}

// The name is embedded in a frozen meta line and then persisted with the thread,
// so an oversized tool input must not turn into a multi-KB instruction.
func TestPlanNudgeName_BoundsLength_Los11b(t *testing.T) {
	got := planNudgeName("/x/" + strings.Repeat("a", 5000) + ".md")
	if runes := []rune(got); len(runes) > maxLoopPathRunes+1 {
		t.Fatalf("planNudgeName returned %d runes, want at most %d", len(runes), maxLoopPathRunes+1)
	}
}

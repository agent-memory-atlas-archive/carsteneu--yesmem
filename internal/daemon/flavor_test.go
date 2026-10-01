package daemon

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The narrative worker derives the session flavor from the narrative's first
// sentence so every extracted session (Claude Code AND opencode) carries a
// flavor — not only those covered by the (config-disabled) fork pipeline.
func TestDeriveSessionFlavor(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"first sentence", "The arc is closed. Everything merged. More text follows.", "The arc is closed."},
		{"no period until end", "One long line without sentence boundary", "One long line without sentence boundary"},
		{"long first sentence capped", "This session covered the proxy pipeline from briefing injection over stub cycles up to keepalive pings and everything in between was verified.", ""},
		{"empty narrative", "", ""},
		{"leading whitespace", "  Starts with spaces. Second.", "Starts with spaces."},
		{"abbreviation is not sentence end", "Der Testlauf mit z.B. DeepSeek brachte Klarheit. Danach war Feierabend.", "Der Testlauf mit z.B. DeepSeek brachte Klarheit."},
		{"umlaut at cap boundary stays valid UTF-8", strings.Repeat("ä", 41) + "b noch mehr Text ohne Punkt", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveSessionFlavor(tc.in)
			if tc.in == "" {
				if got != "" {
					t.Fatalf("empty narrative must yield empty flavor, got %q", got)
				}
				return
			}
			if tc.want == "" {
				if got == "" {
					t.Fatal("expected non-empty flavor for non-empty input")
				}
				if len(got) > 80 {
					t.Fatalf("flavor exceeds 80 chars: %q (%d)", got, len(got))
				}
				if !utf8.ValidString(got) {
					t.Fatalf("flavor is not valid UTF-8: %q", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

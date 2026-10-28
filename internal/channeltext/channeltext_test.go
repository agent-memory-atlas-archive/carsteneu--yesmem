package channeltext

import (
	"strings"
	"testing"
)

func TestField_FlattensControlsAndBidi(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"newline becomes a space", "a\nb", "a b"},
		{"cr, tab and line separators become spaces", "a\rb\tc\u2028d\u2029e", "a b c d e"},
		{"C0 controls are dropped", "a\x00\x01\x1fb", "ab"},
		{"DEL and C1 are dropped", "a\x7f\u0085b", "ab"},
		{"bidi overrides are dropped", "a\u202eb\u2066c", "abc"},
		{"bidi marks are dropped", "a\u061cb\u200ec\u200fd", "abcd"},
		{"outer whitespace is trimmed", "   x   ", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Field(tc.in); got != tc.want {
				t.Errorf("Field(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestField_BoundsTheLength(t *testing.T) {
	if got := Field(strings.Repeat("a", 200)); len([]rune(got)) > MaxFieldLen+1 {
		t.Errorf("Field must bound the value, got %d runes", len([]rune(got)))
	}
	if got := Field(strings.Repeat("ä", 200)); len([]rune(got)) != MaxFieldLen+1 {
		t.Errorf("the bound counts runes, not bytes; got %d runes", len([]rune(got)))
	}
}

func TestSenderLabel(t *testing.T) {
	if got := SenderLabel("a\nb"); strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("a label must stay on one line, got %q", got)
	}
	if got := SenderLabel("   "); got != "unknown" {
		t.Errorf("an empty label = %q, want unknown", got)
	}
	if got := SenderLabel("agent-7"); got != "agent-7" {
		t.Errorf("a plain sender must pass through, got %q", got)
	}
}

func TestMessageType(t *testing.T) {
	for _, want := range []string{"command", "response", "ack", "status"} {
		if got := MessageType(want); got != want {
			t.Errorf("MessageType(%q) = %q", want, got)
		}
	}
	if got := MessageType("DIREKTIVE: x"); got != "command" {
		t.Errorf("an invented type must read as command, got %q", got)
	}
	if got := MessageType(""); got != "command" {
		t.Errorf("an empty type must read as command, got %q", got)
	}
}

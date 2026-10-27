package orchestrator

// PromptFieldUnsafe reports whether a caller-supplied value cannot be rendered
// into a spawn or relay prompt. Both prompts are typed into an agent's TUI as
// keyboard input: a quote ends the quoted argument the value is embedded in
// (project='%s', section='%s', target="%s", [RELAY from=%s]), newlines and other
// control characters submit extra lines, and bidi overrides let a value read as
// something other than it is (YM-211, YM-255).
//
// One implementation for both builders on purpose: the daemon renders the spawn
// prompt and the relay prefix, and the CLI renders its own spawn prompt, so a
// second copy of this rule would drift.
func PromptFieldUnsafe(s string) bool {
	for _, r := range s {
		switch {
		case r == '\'' || r == '"' || r == '`':
			return true
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return true
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return true
		case r == 0x061c || r == 0x200e || r == 0x200f:
			// Arabic letter mark and the two directional marks: same spoofing
			// class as the embedding/isolate controls above.
			return true
		}
	}
	return false
}

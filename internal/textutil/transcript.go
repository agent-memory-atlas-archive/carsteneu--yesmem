package textutil

import "strings"

// IsRawBashTranscript reports whether content is a raw command/stderr
// transcript (session artifact) rather than extracted durable knowledge.
// Matches both deterministic hook formats ("Bash error: `cmd` → output",
// legacy German "Bash-Fehler: `cmd` → Ausgabe").
func IsRawBashTranscript(content string) bool {
	s := strings.TrimSpace(content)
	return strings.HasPrefix(s, "Bash error: `") || strings.HasPrefix(s, "Bash-Fehler: `")
}

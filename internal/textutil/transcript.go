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

// failureOriginPrefixes maps the leading token of a hook-learned failure
// transcript to the tool that failed. Producers write the raw tool name as
// prefix — "<Tool> error: ..." from the Go hook (internal/hooks/failure.go) and
// "<tool> error: ..." with the lower-cased harness tool name from the opencode
// failure hook (plugins/opencode-yesmem-v2/failure_learn.ts), whose shell tool is
// called "shell". The German "Fehler" variants come from an older producer.
var failureOriginPrefixes = []struct {
	prefix string
	origin string
}{
	{"bash error:", "bash"},
	{"bash-fehler:", "bash"},
	{"shell error:", "bash"},
	{"read error:", "read"},
	{"read-fehler:", "read"},
	{"edit error:", "edit"},
	{"webfetch error:", "webfetch"},
	{"webfetch-fehler:", "webfetch"},
}

// FailureOriginTool returns the tool whose failure produced content, or "" when
// content is not a tool failure transcript. Only the leading token is inspected,
// so an ordinary lesson that happens to mention "bash error:" mid-sentence is
// not misclassified.
func FailureOriginTool(content string) string {
	lower := strings.ToLower(strings.TrimSpace(content))
	for _, fp := range failureOriginPrefixes {
		if strings.HasPrefix(lower, fp.prefix) {
			return fp.origin
		}
	}
	return ""
}

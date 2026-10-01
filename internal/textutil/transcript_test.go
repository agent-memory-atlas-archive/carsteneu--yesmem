package textutil

import (
	"testing"
)

func TestIsRawBashTranscript(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"hook transcript", "Bash error: `cp -p ~/.opencode/bin/opencode-x.bak ~/.opencode/bin/opencode` → Exit code 1 | cp: cannot stat", true},
		{"hook transcript truncated cmd", "Bash error: `for f in $(find /tmp -name x 2>/dev/null); do echo -n \"$f: \"; nm -D \"$f\" ...` → Exit code ", true},
		{"german hook transcript", "Bash-Fehler: `go test ./internal/storage/ -count=1 -v 2>&1` → Exit code 1 === RUN", true},
		{"leading whitespace", "\n  Bash error: `ls -la` → Exit code 2", true},
		{"legit lesson mentioning exit code", "`pytest | tail` MASKS the exit code — a failing test suite can appear green and trigger a premature commit; future gates must use `pipefail`.", false},
		{"legit starts with Bash commands", "Bash commands must always be single-quoted in JSON hooks", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := IsRawBashTranscript(tc.content); got != tc.want {
			t.Errorf("%s: IsRawBashTranscript = %v, want %v", tc.name, got, tc.want)
		}
	}
}

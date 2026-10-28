package proxy

import (
	"strings"
	"testing"
)

// YM-400: the project label sits in the same journal line as the thread id and
// comes from client text as well. The two "working directory:" branches of
// extractWorkingDirectoryFromText stop at a newline; the codex <cwd> branch did
// not, so a <cwd> value could end the journal line it was printed into.
func TestLos57bExtractWorkingDirectoryFlattensNewline(t *testing.T) {
	cases := map[string]string{
		"cwd tag":     "<cwd>/tmp/proj\nFORGED-JOURNAL-LINE</cwd>",
		"claude form": "Primary working directory: /tmp/proj\nFORGED-JOURNAL-LINE",
		"opencode":    "Working directory: /tmp/proj\nFORGED-JOURNAL-LINE",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got := extractWorkingDirectoryFromText(text)
			if strings.ContainsAny(got, "\n\r") {
				t.Fatalf("working directory kept a line break: %q", got)
			}
			if !strings.Contains(got, "/tmp/proj") {
				t.Fatalf("working directory lost the path: %q", got)
			}
		})
	}
}

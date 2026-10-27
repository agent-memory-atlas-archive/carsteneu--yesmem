package codescan

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for the duration of fn. The output
// is restored to os.Stderr explicitly: a nil writer makes every later
// log.Printf in this package panic.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	fn()
	return buf.String()
}

// A cache file name is directory content, not our text: a name carrying a line
// break must not be able to forge a second log line (YM-277 / DEV-DT45).
func TestRunCBMGCGated_QuotesClientPathsInLog(t *testing.T) {
	dir := t.TempDir()
	forged := "forged: everything fine"
	path := filepath.Join(dir, "yesmem\n"+forged+".db")
	if err := os.WriteFile(path, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := []CBMGCFinding{{Path: path, SizeBytes: 2, Reason: "orphan"}}

	out := captureLog(t, func() { runCBMGCGated(findings, true) })

	if out == "" {
		t.Fatal("dry run logged nothing")
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "forged:") {
			t.Fatalf("client path forged a log line: %q (full output %q)", line, out)
		}
	}
	if !strings.Contains(out, `\n`) {
		t.Fatalf("line break in the path was not escaped: %q", out)
	}
}

// The same for the real deletion branch, which logs the outcome.
func TestRunCBMGCGated_QuotesClientPathsOnRemove(t *testing.T) {
	dir := t.TempDir()
	forged := "forged: removed by hand"
	path := filepath.Join(dir, "yesmem\n"+forged+".db")
	if err := os.WriteFile(path, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := []CBMGCFinding{{Path: path, SizeBytes: 2, Reason: "orphan"}}

	out := captureLog(t, func() { runCBMGCGated(findings, false) })

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("finding was not removed: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "forged:") {
			t.Fatalf("client path forged a log line: %q (full output %q)", line, out)
		}
	}
}

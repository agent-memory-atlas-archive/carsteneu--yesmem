package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// TestLos56hListDocsKeepsEverySourceOnOneLine covers YM-389: the row printed the source
// name and version raw. ValidSourceName refuses separators and a leading dash but not a
// line break, so a name from 'add-docs --name' could end the row and pass the rest of
// itself off as another line of the table.
func TestLos56hListDocsKeepsEverySourceOnOneLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedDocsDB(t, home, storage.DocSource{
		Name: "guide\nforged: extra row", Version: "1.0", Project: "p",
	})

	out := captureOutput(t, func() { runListDocs([]string{"--project", "p"}) })
	for _, forged := range []string{"\nforged: extra row"} {
		if strings.Contains(out, forged) {
			t.Errorf("the source name ended the output line, got:\n%s", out)
		}
	}
	if !strings.Contains(out, "guide") {
		t.Errorf("the row must still name the source, got:\n%s", out)
	}
}

// TestLos56hLogOutputLinesSanitizesEveryLine covers the git diagnostic sink: the clone
// and pull helpers printed the child's output raw, so an escape sequence or a control
// byte from a remote's message reached the terminal. The line structure stays, because a
// multi-line diagnostic is what the operator needs.
func TestLos56hLogOutputLinesSanitizesEveryLine(t *testing.T) {
	out := los04iCaptureLog(t, func() {
		logOutputLines("  git: ", []byte("fatal: no access\n\x1b[2Jcleared\x07\ntrailing\n"))
	})
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("control bytes must not survive, got %q", out)
	}
	// Every line keeps the prefix, so each one is attributable. The escape's parameter
	// bytes ("[2J") stay as text: a flattened control byte cannot be told apart from the
	// rest of the sequence, and what matters is that the sequence no longer acts.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("the diagnostic must keep its three lines, got %q", out)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "  git: ") {
			t.Errorf("every output line must carry the prefix, got %q in %q", line, out)
		}
	}
	if !strings.Contains(out, "fatal: no access") || !strings.Contains(out, "trailing") {
		t.Errorf("the diagnostic text must survive, got %q", out)
	}
}

// TestLos56hCloneFailureKeepsTheURLCitable covers the clone report: the URL came from
// --url and was printed raw, both in the "Cloning" line and in the failure line.
func TestLos56hCloneFailureKeepsTheURLCitable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	forged := "no-such-repo\nwarn: forged second line"
	url := filepath.Join(t.TempDir(), forged)

	out := los04iCaptureLog(t, func() {
		if clonePath := cloneOrPullRepo("los56h-clone", url); clonePath != "" {
			t.Errorf("cloning a nonexistent path must fail, got %q", clonePath)
		}
	})
	if strings.Contains(out, "\nwarn: forged second line") {
		t.Errorf("the URL ended the log line, got:\n%s", out)
	}
}

// TestLos56hExportQuotesThePath covers the export and import reports: the path is
// caller-supplied and was printed raw.
func TestLos56hExportQuotesThePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedDocsDB(t, home)
	path := filepath.Join(t.TempDir(), "export\nforged: second line.json")

	args := os.Args
	os.Args = []string{"yesmem", "export", path}
	t.Cleanup(func() { os.Args = args })

	out := captureOutput(t, func() { runExport() })
	if strings.Contains(out, "\nforged: second line.json") {
		t.Errorf("the export path ended the output line, got:\n%s", out)
	}
	if !strings.Contains(out, "Exported") {
		t.Errorf("the report must still name the result, got:\n%s", out)
	}
}

// TestLos56hCBMFindingKeepsTheLine covers the cbm-gc report: the path and the reason
// were printed on two lines, so a line break inside either one invented a third.
func TestLos56hCBMFindingKeepsTheLine(t *testing.T) {
	line := cbmFindingLine("1.0 MB", "/tmp/a\nforged: reason", "orphan\nforged: path")
	if strings.Contains(line, "\nforged:") {
		t.Errorf("the finding ended a line inside a value, got %q", line)
	}
	if !strings.Contains(line, "forged") {
		t.Errorf("the values must still be reported, got %q", line)
	}
}

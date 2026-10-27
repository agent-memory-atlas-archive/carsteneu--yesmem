package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// los07dFakeConverter shadows pdftotext with a converter that never finishes.
// The shell stays while its sleep runs, so the inherited stdout pipe stays open
// after the kill: the run is only bounded when the deadline and the pipe close
// are both handled.
func los07dFakeConverter(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	// The grandchild has to outlive the 2s WaitDelay or the test would not probe
	// the pipe-close path; keeping it at 5s bounds the orphan it leaves behind.
	script := "#!/bin/sh\nsleep 5\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "pdftotext"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake pdftotext: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// YM-324 [DEV-DT50]: pdftotext ran without a deadline. A converter that hangs on
// a malformed PDF stalled the ingest of every following document, and inside the
// daemon's doc-sync pass it stalled the tick itself.
func TestDocIngest_PdfConverterIsBoundedLos07d(t *testing.T) {
	previous := pdfExtractTimeout
	pdfExtractTimeout = 150 * time.Millisecond
	t.Cleanup(func() { pdfExtractTimeout = previous })

	los07dFakeConverter(t)

	dir := t.TempDir()
	pdf := filepath.Join(dir, "hang.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4\n"), 0644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	type outcome struct {
		content string
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		content, err := readFile(dir, pdf)
		done <- outcome{content, err}
	}()

	select {
	case o := <-done:
		if o.err == nil {
			t.Fatalf("readFile accepted content %q from a converter that never finished", o.content)
		}
		if !strings.Contains(strings.ToLower(o.err.Error()), "timed out") {
			t.Errorf("error = %v, want one that names the timeout", o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readFile did not return within 10s: pdftotext runs without a deadline")
	}
}

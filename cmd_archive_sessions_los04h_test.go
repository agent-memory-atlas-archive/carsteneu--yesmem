package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The archive directory is built from the local data directory, not from client
// input, but it is still a path on an output line: a HOME carrying a newline
// splits the line and the rest reads as its own archive entry. Quoted like the
// other paths of this output family (YM-366, escaped from YM-332).
func TestRunArchiveSessions_QuotesTheArchiveDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home\nwarn: forged archive line")
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	prevArgs, prevStdout := os.Args, os.Stdout
	t.Cleanup(func() {
		os.Args, os.Stdout = prevArgs, prevStdout
	})
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"yesmem", "archive-sessions", "--dry-run"}
	os.Stdout = writer
	runArchiveSessions()
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	os.Stdout = prevStdout
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	reader.Close()

	printed := string(out)
	archiveDir := filepath.Join(dataDir, "archive")
	if !strings.Contains(printed, strconv.Quote(archiveDir)) {
		t.Fatalf("the archive directory is not quoted: %q", printed)
	}
	if strings.Contains(printed, "\nwarn: forged") {
		t.Fatalf("the archive path forged an output line:\n%s", printed)
	}
}

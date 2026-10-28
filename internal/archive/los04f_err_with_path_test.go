package archive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The path in an archive error comes from the client (a session file name), and
// the message ends up in daemon.log. A raw path there would split the log line
// and let the rest pose as its own entry (YM-311).
func TestErrWithPathQuotesThePathAndKeepsTheReason(t *testing.T) {
	path := "/tmp/proj/sess\nwarn: forged log line.jsonl"

	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{
			name:   "path error",
			err:    &os.PathError{Op: "open", Path: path, Err: syscall.EISDIR},
			reason: "is a directory",
		},
		{
			name:   "link error",
			err:    &os.LinkError{Op: "rename", Old: path + ".tmp", New: path, Err: syscall.EEXIST},
			reason: "file exists",
		},
		{
			name:   "plain error",
			err:    errors.New("boom"),
			reason: "boom",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errWithPath("create", path, tc.err).Error()
			if strings.Contains(got, "\n") {
				t.Fatalf("error text carries a raw newline: %q", got)
			}
			if !strings.Contains(got, strconv.Quote(path)) {
				t.Fatalf("path is not quoted: %q", got)
			}
			if !strings.Contains(got, tc.reason) {
				t.Fatalf("reason lost: %q", got)
			}
		})
	}

	// The reason must survive as a class, not only as text: callers ask whether
	// a file is missing.
	missing := errWithPath("stat source", path, &os.PathError{Op: "stat", Path: path, Err: syscall.ENOENT})
	if !errors.Is(missing, os.ErrNotExist) {
		t.Fatalf("error class lost: %v", missing)
	}
}

// WriteSessionDoc routes its own error paths through the same quoting. Its name
// comes from a session id, so a real rename failure must stay one line too. A
// renamed-to directory is the destination form that fails after the temporary
// file was written.
func TestWriteSessionDocRenameErrorIsQuoted(t *testing.T) {
	a := New(t.TempDir())
	project, name := "proj", "sess\nwarn: forged log line.md"

	dest, err := a.SessionDocPath(project, name)
	if err != nil {
		t.Fatalf("session doc path: %v", err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "occupant"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = a.WriteSessionDoc(project, name, func(w io.Writer) error {
		_, writeErr := w.Write([]byte("# doc\n"))
		return writeErr
	})
	if err == nil {
		t.Fatal("expected the rename onto a directory to fail")
	}
	msg := err.Error()
	if strings.Contains(msg, "\n") {
		t.Fatalf("error text carries a raw newline: %q", msg)
	}
	if !strings.Contains(msg, strconv.Quote(dest)) {
		t.Fatalf("destination path is not quoted: %q", msg)
	}
}

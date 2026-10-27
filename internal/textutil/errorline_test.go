package textutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A filesystem error carries its path raw: os.PathError prints the path between
// op and reason, so a path with a newline ends the log line (YM-332).
func TestErrorLineQuotesThePathOfAPathError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home\nwarn: forged line", "yesmem.db")
	err := &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}

	got := ErrorLine(err)

	if strings.Contains(got, "\n") {
		t.Fatalf("the path ended the line: %q", got)
	}
	if !strings.Contains(got, strings.ReplaceAll(path, "\n", `\n`)) {
		t.Fatalf("the path is missing from the rendered error: %q", got)
	}
	if !strings.Contains(got, os.ErrPermission.Error()) {
		t.Fatalf("the reason is missing from the rendered error: %q", got)
	}
}

// The wrapper text tells two errors of the same shape apart ("open runtime db"
// against "open messages db"), so quoting the path must not replace it.
func TestErrorLineKeepsTheWrapperText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home\nwarn: forged line", "runtime.db")
	err := fmt.Errorf("open runtime db: %w", &os.PathError{Op: "create", Path: path, Err: os.ErrExist})

	got := ErrorLine(err)

	if !strings.Contains(got, "open runtime db") {
		t.Fatalf("the wrapper text is gone: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("the path ended the line: %q", got)
	}
}

// A rename carries two paths, and either of them can hold a newline. One path
// can be the prefix of the other, so both must survive quoting.
func TestErrorLineQuotesBothPathsOfALinkError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "from\nwarn: forged old")
	oldPath := dir
	newPath := filepath.Join(dir, "to\nwarn: forged new")
	err := &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: os.ErrExist}

	got := ErrorLine(err)

	if strings.Contains(got, "\n") {
		t.Fatalf("a path ended the line: %q", got)
	}
	for _, want := range []string{
		strings.ReplaceAll(oldPath, "\n", `\n`),
		strings.ReplaceAll(newPath, "\n", `\n`),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("a path is missing from the rendered error (%q): %q", want, got)
		}
	}
}

// Errors without a path still reach a log line, so a line break in their text
// must not survive either. The format characters SingleLine drops include the
// unicode line separators, which some readers treat as a line end.
func TestErrorLineFlattensAnErrorWithoutPath(t *testing.T) {
	got := ErrorLine(os.ErrNotExist)

	if strings.Contains(got, "\n") {
		t.Fatalf("the message kept a line break: %q", got)
	}
	if got != os.ErrNotExist.Error() {
		t.Fatalf("ErrorLine changed a one-line message: %q", got)
	}

	got = ErrorLine(errors.New("first\u2028second\u2029third"))
	if strings.ContainsAny(got, "\u2028\u2029") {
		t.Fatalf("a unicode line separator survived: %q", got)
	}
}

// A reason that itself carries a line break must not end the line, even inside a
// filesystem error.
func TestErrorLineFlattensTheReasonOfAPathError(t *testing.T) {
	err := &os.PathError{Op: "open", Path: "/tmp/x.db", Err: errors.New("disk says\nwarn: forged line")}

	got := ErrorLine(err)

	if strings.Contains(got, "\n") {
		t.Fatalf("the reason ended the line: %q", got)
	}
	if !strings.Contains(got, "disk says") || !strings.Contains(got, "warn: forged line") {
		t.Fatalf("the reason was dropped instead of flattened: %q", got)
	}
}

// A fatal message is read by a human looking for the cause, so the driver part at
// the end of a long error must not be cut off.
func TestErrorLineKeepsLongMessages(t *testing.T) {
	reason := "the driver says: disk I/O error " + strings.Repeat("a", 1500) + " END OF REASON"
	got := ErrorLine(&os.PathError{Op: "write", Path: "/tmp/x.db", Err: errors.New(reason)})

	if !strings.Contains(got, "the driver says: disk I/O error") {
		t.Fatalf("the head of the message is gone: %q", got)
	}
	if !strings.Contains(got, "END OF REASON") {
		t.Fatalf("the end of the message is gone: %q", got)
	}
}

// A joined error can hold a typed nil. Rendering it must not panic: the value
// reaches the dispatcher's fatal lines from install and uninstall.
func TestErrorLineSurvivesATypedNilInsideAJoin(t *testing.T) {
	err := errors.Join(errors.New("config: missing key"), (*os.PathError)(nil))

	got := ErrorLine(err)

	if strings.Contains(got, "\n") {
		t.Fatalf("the message ended the line: %q", got)
	}
}

// An empty path must not stall the replacement.
func TestErrorLineToleratesAnEmptyPath(t *testing.T) {
	done := make(chan string, 1)
	go func() {
		done <- ErrorLine(&os.PathError{Op: "open", Path: "", Err: os.ErrPermission})
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, os.ErrPermission.Error()) {
			t.Fatalf("the reason is missing: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an empty path stalled ErrorLine")
	}
}

// A nil *os.PathError in an error interface is not nil, and rendering it must not
// panic: a log helper is the last place that may take the process down. Inside a
// wrapper the message is still renderable, so it must not be dropped.
func TestErrorLineToleratesATypedNilError(t *testing.T) {
	var typedNil *os.PathError
	if got := ErrorLine(typedNil); got != "" {
		t.Fatalf("a typed nil error rendered as %q", got)
	}
	if got := ErrorLine(nil); got != "" {
		t.Fatalf("nil rendered as %q", got)
	}

	wrapped := ErrorLine(fmt.Errorf("open db: %w", typedNil))
	if !strings.HasPrefix(wrapped, "open db:") {
		t.Fatalf("the wrapper text of a typed nil is gone: %q", wrapped)
	}
	if strings.Contains(wrapped, "\n") {
		t.Fatalf("the wrapper of a typed nil ended the line: %q", wrapped)
	}
}

// A wrapper may quote the path itself, and a quoted path must not be quoted a
// second time: the line stays readable and keeps exactly one escaped form.
func TestErrorLineDoesNotQuoteAnAlreadyQuotedPathTwice(t *testing.T) {
	path := "/tmp/los04i/x.db"
	err := fmt.Errorf("stat %q: %w", path, &os.PathError{Op: "lstat", Path: path, Err: os.ErrNotExist})

	got := ErrorLine(err)

	if strings.Contains(got, `""`) {
		t.Fatalf("a path was quoted twice: %q", got)
	}
	if n := strings.Count(got, strconv.Quote(path)); n != 2 {
		t.Fatalf("want the path quoted twice (wrapper and filesystem error), got %d: %q", n, got)
	}
}

// A path that contains another quoted path must not be split by the replacement.
func TestErrorLineKeepsQuotedSpansIntact(t *testing.T) {
	long := "/tmp/los04i/a/b.log"
	err := fmt.Errorf("copy %q: %w", long, &os.LinkError{Op: "rename", Old: "/tmp/los04i/a", New: long, Err: os.ErrExist})

	got := ErrorLine(err)

	if strings.Contains(got, `""`) {
		t.Fatalf("a quoted span was broken: %q", got)
	}
	if !strings.Contains(got, strconv.Quote(long)) {
		t.Fatalf("the longer path lost its quoted form: %q", got)
	}
}

// A path can itself contain a quote. strconv.Quote escapes it as \", and that
// escaped quote is not a delimiter: the following raw occurrence must still be
// quoted.
func TestErrorLineHandlesQuotesInsideThePath(t *testing.T) {
	path := "/tmp/los04i/a\"b\nx"
	err := fmt.Errorf("stat %q: %w", path, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission})

	got := ErrorLine(err)

	if strings.Contains(got, "\n") {
		t.Fatalf("the path ended the line: %q", got)
	}
	if n := strings.Count(got, strconv.Quote(path)); n != 2 {
		t.Fatalf("want the path quoted twice, got %d: %q", n, got)
	}
}

// errors.Join keeps several errors, and a path in any of them must be quoted:
// the join form has no Unwrap() error, only Unwrap() []error.
func TestErrorLineQuotesPathsOfJoinedErrors(t *testing.T) {
	first := filepath.Join(t.TempDir(), "one\nwarn: forged first")
	second := filepath.Join(t.TempDir(), "two\nwarn: forged second")
	err := errors.Join(
		&os.PathError{Op: "open", Path: first, Err: os.ErrPermission},
		&os.PathError{Op: "stat", Path: second, Err: os.ErrNotExist},
	)

	got := ErrorLine(err)

	if strings.Contains(got, "\n") {
		t.Fatalf("a joined path ended the line: %q", got)
	}
	for _, p := range []string{first, second} {
		if !strings.Contains(got, strings.ReplaceAll(p, "\n", `\n`)) {
			t.Fatalf("the joined path is missing from the rendered error (%q): %q", p, got)
		}
	}
}

// A path directly behind a backslash must still be quoted: the escape pair is not
// a reason to skip the position.
func TestErrorLineQuotesAPathBehindABackslash(t *testing.T) {
	path := "/tmp/los04i/behind\nwarn: forged behind"
	err := fmt.Errorf("note \\%s: %w", path, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission})

	got := ErrorLine(err)

	if strings.Contains(got, "\n") {
		t.Fatalf("the path ended the line: %q", got)
	}
	if n := strings.Count(got, strconv.Quote(path)); n != 2 {
		t.Fatalf("want the path quoted twice, got %d: %q", n, got)
	}
}

// An error whose Unwrap method panics must not take the process down either.
func TestErrorLineSurvivesAPanickingUnwrap(t *testing.T) {
	got := ErrorLine(panickingUnwrap{})
	if got == "" {
		t.Fatal("a panicking Unwrap silenced the whole line")
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("the fallback ended the line: %q", got)
	}
}

type panickingUnwrap struct{}

func (panickingUnwrap) Error() string { return "broken error" }
func (panickingUnwrap) Unwrap() error { return errors.New((*os.PathError)(nil).Error()) }

package textutil

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// errorLineRunes bounds a pathological line, and errorLineGap marks the two
// halves that survive: the failing operation sits at the start, the reason from
// the driver or the database at the end.
const (
	errorLineRunes = 1000
	errorLineGap   = " [...] "
)

// ErrorLine renders err for a log or output line. A path carried by an
// *os.PathError or an *os.LinkError is quoted in place, because %v prints it
// raw: a path built from HOME or from a client value can hold a newline, which
// would end the line and pass the rest off as its own log entry (YM-332). The
// rest of the message stays, so the wrapper text that tells two errors of the
// same shape apart ("open runtime db" against "open messages db") survives.
// Everything else goes through flatten for the same reason.
func ErrorLine(err error) (line string) {
	if err == nil || typedNil(err) {
		return ""
	}
	// A joined error can hold a typed nil, and a third-party error type can panic
	// in Error or Unwrap. A log helper must not take the process down for that, so
	// everything below runs under one recover.
	defer func() {
		if recover() != nil {
			line = fmt.Sprintf("error not renderable (%T)", err)
		}
	}()
	return errorLineText(quotePathsInLine(err.Error(), errorPaths(err)))
}

// errorLineText flattens the message and, if it is too long, keeps its head and
// its tail: unlike a plain cap this keeps the reason that the operation failed.
func errorLineText(msg string) string {
	msg = flatten(msg)
	runes := []rune(msg)
	if len(runes) <= errorLineRunes {
		return msg
	}
	gap := []rune(errorLineGap)
	tail := errorLineRunes / 3
	head := errorLineRunes - tail - len(gap)
	return string(runes[:head]) + errorLineGap + string(runes[len(runes)-tail:])
}

// quotePathsInLine replaces every raw occurrence of a path with its quoted form
// in one pass. An occurrence inside an already quoted span stays as it is: the
// wrapper may have printed the path with %q itself, and quoting it twice would
// read as a broken path. The paths arrive sorted longest first, so a path that
// starts with another one wins.
func quotePathsInLine(msg string, paths []string) string {
	if len(paths) == 0 {
		return msg
	}
	var b strings.Builder
	b.Grow(len(msg) + 16)
	inQuotes := false
	escaped := false
	for i := 0; i < len(msg); {
		if !inQuotes {
			if p, ok := leadingPath(msg[i:], paths); ok {
				b.WriteString(strconv.Quote(p))
				i += len(p)
				escaped = false
				continue
			}
		}
		switch c := msg[i]; {
		// An escaped quote belongs to a path and does not open a span. The state
		// only marks the byte after the backslash, so a path that starts there is
		// still found by the check above.
		case escaped:
			escaped = false
			b.WriteByte(c)
			i++
		case c == '\\':
			escaped = true
			b.WriteByte(c)
			i++
		case c == '"':
			inQuotes = !inQuotes
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// leadingPath reports which of the paths starts s.
func leadingPath(s string, paths []string) (string, bool) {
	for _, p := range paths {
		if p != "" && strings.HasPrefix(s, p) {
			return p, true
		}
	}
	return "", false
}

// typedNil reports an error interface whose own type is a nil *os.PathError or
// *os.LinkError. Rendering that value would panic on Error(), and a log helper is
// the last place that may take the process down.
func typedNil(err error) bool {
	switch e := err.(type) {
	case *os.PathError:
		return e == nil
	case *os.LinkError:
		return e == nil
	}
	return false
}

// errorPaths returns the paths a filesystem error carries. The chain is walked
// here and not with errors.As: As calls Unwrap on a typed nil *os.PathError while
// it looks for a *os.LinkError, which dereferences nil. Both unwrap forms are
// followed, so a joined error keeps its paths as well.
func errorPaths(err error) []string {
	var paths []string
	queue := []error{err}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		switch v := e.(type) {
		case *os.PathError:
			if v == nil {
				continue
			}
			paths = append(paths, v.Path)
			queue = append(queue, v.Err)
		case *os.LinkError:
			if v == nil {
				continue
			}
			paths = append(paths, v.Old, v.New)
			queue = append(queue, v.Err)
		default:
			switch u := e.(type) {
			case interface{ Unwrap() []error }:
				queue = append(queue, u.Unwrap()...)
			case interface{ Unwrap() error }:
				if inner := u.Unwrap(); inner != nil {
					queue = append(queue, inner)
				}
			}
		}
	}
	// Longest first: one path can be the prefix of the other.
	sort.SliceStable(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	return paths
}

package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/codescan"
)

const los04gForgedProject = "los04g\nwarn: forged wiki line"

// los04gCaptureLog redirects the standard logger into a buffer for one test.
func los04gCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// The project name reaches the wiki tick from the learnings table, which an agent
// fills through remember(). A newline in it must not split a daemon log line
// (YM-332).
func TestRunWikiOnce_QuotesTheProjectInEveryLogLine(t *testing.T) {
	store := mustStore(t)
	insertLearningForProject(t, store, los04gForgedProject)
	insertSessionForProject(t, store, los04gForgedProject, time.Now().Format(time.RFC3339))

	logged := los04gCaptureLog(t)
	runWikiOnce(context.Background(), store, t.TempDir(), codescan.NewCachedScanner(scanDouble{}))

	got := logged.String()
	if !strings.Contains(got, "wiki-tick:") {
		t.Fatalf("the tick logged nothing about the project: %q", got)
	}
	if strings.Contains(got, "\nwarn: forged") {
		t.Fatalf("a client project name forged a log entry:\n%s", got)
	}
	// The warning path above quotes the name already; the tick's own line is what
	// this test is about.
	var tickLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "wiki-tick:") {
			tickLine = line
		}
	}
	if !strings.Contains(tickLine, strconv.Quote(los04gForgedProject)) {
		t.Fatalf("the tick line does not quote the project: %q", tickLine)
	}
}

// Quoting the project is not enough when the error text carries the same path:
// %v of an *os.PathError prints it unquoted. A name at the file-name limit passes
// the wiki naming and then fails the marker write with ENAMETOOLONG.
func TestRunWikiOnce_QuotesTheErrorPathOfAFailedMarkerWrite(t *testing.T) {
	store := mustStore(t)
	project := strings.Repeat("a", 250) + "\nwarn: forged mark line"
	insertLearningForProject(t, store, project)
	insertSessionForProject(t, store, project, time.Now().Format(time.RFC3339))

	logged := los04gCaptureLog(t)
	runWikiOnce(context.Background(), store, t.TempDir(), codescan.NewCachedScanner(scanDouble{}))

	got := logged.String()
	if !strings.Contains(got, "wiki-tick: mark") {
		t.Fatalf("the failed marker write was not logged: %q", got)
	}
	if strings.Contains(got, "\nwarn: forged") {
		t.Fatalf("an error path forged a log entry:\n%s", got)
	}
	// The reason must survive the path quoting, not be replaced by an empty text.
	if !strings.Contains(got, "file name too long") {
		t.Fatalf("the error reason was dropped:\n%s", got)
	}
}

// A plain error has no path to quote; its own line breaks must still not reach
// the log, and collapsing them must not glue the words together.
func TestWikiErrorText_KeepsAPlainErrorOnOneLine(t *testing.T) {
	got := wikiErrorText(errors.New("cannot\nwrite the page\u200b\u00ad"))
	if strings.Contains(got, "\n") {
		t.Fatalf("the text spans more than one line: %q", got)
	}
	if strings.ContainsAny(got, "\u200b\u00ad") {
		t.Fatalf("invisible format characters survived: %q", got)
	}
	if !strings.Contains(got, "cannot write the page") {
		t.Fatalf("the words were glued together: %q", got)
	}
}

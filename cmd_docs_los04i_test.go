package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/ingest"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los04iCaptureLog captures the standard logger, which writes to stderr by
// default, for the duration of fn.
func los04iCaptureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	fn()
	return buf.String()
}

// los04iEscaped is the form a value takes inside a quoted %q field: the line
// break stays visible as the two characters \ and n instead of ending the line.
func los04iEscaped(s string) string {
	return strings.ReplaceAll(s, "\n", `\n`)
}

// The prune report prints the source name, its project and its path from the
// doc_sources table. The name comes from 'add-docs --name' and a name carrying a
// newline ended the line, so the rest read as its own output entry (YM-332,
// escaped from Los 04h).
func TestSyncDocs_PruneReportQuotesNameAndPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gone := filepath.Join(t.TempDir(), "gone")

	rulesName := "rules\nwarn: forged rules line"
	rulesPath := filepath.Join(gone, "CLAUDE\nwarn: forged rules path.md")
	plainName := "plain\nwarn: forged plain line"
	plainPath := filepath.Join(gone, "docs\nwarn: forged plain path")
	seedDocsDB(t, home,
		storage.DocSource{Name: rulesName, Project: "p", Path: rulesPath, IsRules: true},
		storage.DocSource{Name: plainName, Project: "p", Path: plainPath},
	)

	out := captureOutput(t, func() { runSyncDocs([]string{"--all", "--prune"}) })

	for _, forged := range []string{
		"\nwarn: forged rules line", "\nwarn: forged plain line",
		"\nwarn: forged rules path", "\nwarn: forged plain path",
	} {
		if strings.Contains(out, forged) {
			t.Fatalf("a source value forged an output line (%q):\n%s", forged, out)
		}
	}
	for _, want := range []string{
		"Keeping", "Removed",
		los04iEscaped(rulesName), los04iEscaped(plainName),
		los04iEscaped(rulesPath), los04iEscaped(plainPath),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the prune report is missing %q:\n%s", want, out)
		}
	}
}

// A source whose delete fails is reported with its name. The name comes from
// 'add-docs --name', and a newline in it ended the log line (YM-332, escaped
// from Los 04h). A trigger forces the failure without a broken installation.
func TestSyncDocs_PruneFailureQuotesTheSourceName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	name := "boom\nwarn: forged prune line"
	seedDocsDB(t, home, storage.DocSource{Name: name, Project: "p", Path: filepath.Join(t.TempDir(), "gone", "docs")})

	store := openDocsDB(t, home)
	if _, err := store.DB().Exec(`CREATE TRIGGER los04i_block_delete BEFORE DELETE ON doc_sources
		BEGIN SELECT RAISE(ABORT, 'los04i refuses'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	logged := los04iCaptureLog(t, func() { runSyncDocs([]string{"--all", "--prune"}) })

	if !strings.Contains(logged, "prune") {
		t.Fatalf("the failed prune was not reported:\n%s", logged)
	}
	if strings.Contains(logged, "\nwarn: forged prune line") {
		t.Fatalf("the source name forged a log line:\n%s", logged)
	}
	if !strings.Contains(logged, los04iEscaped(name)) {
		t.Fatalf("the source name is missing or unquoted:\n%s", logged)
	}
}

// The skip and sync lines of 'sync-docs' print the source name and the path
// registered through 'add-docs', both client input. A newline in either of them
// ended the log line (YM-332, escaped from Los 04h).
func TestSyncDocs_SkipAndSyncLinesQuoteClientValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	live := filepath.Join(t.TempDir(), "docs\nwarn: forged live path")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatalf("mkdir live source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(live, "guide.md"), []byte("# Guide\n\nBody text.\n"), 0o644); err != nil {
		t.Fatalf("write guide: %v", err)
	}

	noPathName := "nopath\nwarn: forged nopath name"
	relativeName := "relative\nwarn: forged relative name"
	relativePath := "docs\nwarn: forged relative path"
	missingName := "missing\nwarn: forged missing name"
	liveName := "live\nwarn: forged live name"

	seedDocsDB(t, home,
		storage.DocSource{Name: noPathName, Project: "p"},
		storage.DocSource{Name: relativeName, Project: "p", Path: relativePath},
		storage.DocSource{Name: missingName, Project: "p", Path: filepath.Join(t.TempDir(), "gone", "docs")},
		storage.DocSource{Name: liveName, Project: "p", Path: live},
	)

	var logged string
	out := captureOutput(t, func() {
		logged = los04iCaptureLog(t, func() { runSyncDocs([]string{"--all"}) })
	})
	combined := out + logged

	for _, forged := range []string{
		"\nwarn: forged nopath name",
		"\nwarn: forged relative name",
		"\nwarn: forged relative path",
		"\nwarn: forged missing name",
		"\nwarn: forged live name",
		"\nwarn: forged live path",
	} {
		if strings.Contains(combined, forged) {
			t.Fatalf("a client value forged a log line (%q):\n%s", forged, combined)
		}
	}
	for _, want := range []string{
		"Syncing", "is gone", "is relative",
		los04iEscaped(noPathName), los04iEscaped(relativeName), los04iEscaped(relativePath),
		los04iEscaped(missingName), los04iEscaped(liveName), los04iEscaped(live),
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("the sync report is missing %q:\n%s", want, combined)
		}
	}
}

// remove-docs reports the removed source name and the clone directory it deletes,
// both derived from the client-supplied name (YM-332, escaped from Los 04h).
func TestRemoveDocs_QuotesNameAndCloneDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	name := "clone\nwarn: forged clone name"
	seedDocsDB(t, home, storage.DocSource{Name: name, Project: "p", Path: "https://example.invalid/docs"})

	cloneDir, err := ingest.CloneDir(filepath.Join(home, ".claude", "yesmem"), name)
	if err != nil {
		t.Fatalf("clone dir: %v", err)
	}
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatalf("mkdir clone dir: %v", err)
	}

	var logged string
	out := captureOutput(t, func() {
		logged = los04iCaptureLog(t, func() { runRemoveDocs([]string{"--name", name, "--project", "p"}) })
	})
	combined := out + logged

	if strings.Contains(combined, "\nwarn: forged clone name") {
		t.Fatalf("the source name forged an output line:\n%s", combined)
	}
	if strings.Contains(combined, "\nwarn: forged clone name/.claude") {
		t.Fatalf("the clone directory forged an output line:\n%s", combined)
	}
	for _, want := range []string{"Removed", "Clone directory removed", los04iEscaped(name), los04iEscaped(cloneDir)} {
		if !strings.Contains(combined, want) {
			t.Fatalf("remove-docs output is missing %q:\n%s", want, combined)
		}
	}
}

// A failed clone directory removal is reported on one line. The clone directory
// carries the client-supplied name, so a newline in it reaches the line through
// the error of os.RemoveAll. A directory without write permission makes the
// removal fail; root ignores that mode, so this test only runs for a regular user.
func TestRemoveDocs_QuotesAFailedCloneDirRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode this test relies on")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	name := "rmfail\nwarn: forged remove line"
	seedDocsDB(t, home, storage.DocSource{Name: name, Project: "p", Path: "https://example.invalid/docs"})

	cloneDir, err := ingest.CloneDir(filepath.Join(home, ".claude", "yesmem"), name)
	if err != nil {
		t.Fatalf("clone dir: %v", err)
	}
	locked := filepath.Join(cloneDir, "sub")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "blocked.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocked file: %v", err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("chmod sub: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	logged := los04iCaptureLog(t, func() { runRemoveDocs([]string{"--name", name, "--project", "p"}) })

	if !strings.Contains(logged, "remove clone dir") {
		t.Fatalf("the failed removal was not reported:\n%s", logged)
	}
	if strings.Contains(logged, "\nwarn: forged remove line") {
		t.Fatalf("the clone directory forged a log line:\n%s", logged)
	}
	if !strings.Contains(logged, los04iEscaped(cloneDir)) {
		t.Fatalf("the clone directory is missing or unquoted:\n%s", logged)
	}
}

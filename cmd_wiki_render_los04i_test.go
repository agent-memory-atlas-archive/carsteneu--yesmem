package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/textutil"
	"github.com/carsteneu/yesmem/internal/wikidir"
)

// los04iSeedWikiLearning gives the project one learning, so the render claims the
// output directory and prints its status line.
func los04iSeedWikiLearning(t *testing.T, home, project string) {
	t.Helper()
	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	store, err := storage.Open(filepath.Join(dataDir, "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if _, err := store.InsertLearning(&models.Learning{
		Category:   "gotcha",
		Content:    "wiki render quoting test learning",
		Project:    project,
		Confidence: 1,
		CreatedAt:  time.Now(),
		Source:     "claude_suggested",
		Importance: 1,
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}
}

// The ownership error of 'wiki-render' names the output directory, which is
// built from --out or from the project name. A newline in it ended the line that
// main.go prints for the error (YM-332, escaped from Los 04h).
func TestWikiRender_OwnershipErrorQuotesTheOutputDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(t.TempDir(), "proj\nwarn: forged project name")
	los04iSeedWikiLearning(t, home, project)

	out := filepath.Join(t.TempDir(), "wiki\nwarn: forged wiki dir")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}
	if err := wikidir.WriteMarker(out, "/some/other/project"); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	err := runWikiRender([]string{"--project", project, "--out", out, "--json=false"})
	if err == nil {
		t.Fatal("a wiki directory owned by another project must be refused")
	}
	msg := err.Error()
	if strings.Contains(msg, "\nwarn: forged") {
		t.Fatalf("the output directory forged a log line:\n%s", msg)
	}
	if !strings.Contains(msg, los04iEscaped(out)) {
		t.Fatalf("the refused directory is missing from the error: %q", msg)
	}
}

// A marker path that cannot be written still has to name its directory on one
// line: the write error carries the path of the marker. A directory in place of
// the marker file makes the write fail with EISDIR, which holds for every user,
// root included.
func TestWikiRender_MarkerWriteErrorQuotesTheDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(t.TempDir(), "proj")
	los04iSeedWikiLearning(t, home, project)

	out := filepath.Join(t.TempDir(), "wiki\nwarn: forged marker dir")
	if err := os.MkdirAll(filepath.Join(out, wikidir.MarkerFile), 0o755); err != nil {
		t.Fatalf("mkdir marker dir: %v", err)
	}

	err := runWikiRender([]string{"--project", project, "--out", out, "--json=false"})
	if err == nil {
		t.Fatal("an unwritable ownership marker must be reported")
	}
	// The error keeps its chain (%w); the dispatcher renders it with ErrorLine,
	// so the rendered line is what the user sees.
	msg := textutil.ErrorLine(err)
	if strings.Contains(msg, "\nwarn: forged") {
		t.Fatalf("the marker path forged a log line:\n%s", msg)
	}
	// The rendered error also quotes the marker path below the directory, so only
	// the quoted directory itself proves the fix at the output site.
	if !strings.Contains(msg, strconv.Quote(out)) {
		t.Fatalf("the refused directory is not quoted in the marker error: %q", msg)
	}
}

// The status line of 'wiki-render' prints the project, which the client passes
// as --project (YM-332, escaped from Los 04h).
func TestWikiRender_StatusLineQuotesTheProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(t.TempDir(), "proj\nwarn: forged status line")
	los04iSeedWikiLearning(t, home, project)

	out := filepath.Join(t.TempDir(), "wiki")
	printed := captureOutput(t, func() {
		if err := runWikiRender([]string{"--project", project, "--out", out, "--json=false"}); err != nil {
			t.Fatalf("render: %v", err)
		}
	})

	if strings.Contains(printed, "\nwarn: forged status line") {
		t.Fatalf("the project forged an output line:\n%s", printed)
	}
	if !strings.Contains(printed, los04iEscaped(project)) {
		t.Fatalf("the status line does not name the project: %q", printed)
	}
}

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/wikidir"
)

// A failed marker write leaves the ownership of the directory unproven, and the
// directory may hold another project's wiki whose marker cannot be read. The tick
// must not render into it (YM-366, escaped from YM-311).
func TestRunWikiOnce_DoesNotRenderWhenTheMarkerWriteFails(t *testing.T) {
	s := newWikiTickTestStore(t)
	projDir := fixtureProjectDir(t, "los04h/marker-owned")
	insertLearningForProject(t, s, projDir)
	insertSessionForProject(t, s, projDir, time.Now().Format(time.RFC3339))

	outRoot := t.TempDir()
	out := filepath.Join(outRoot, wikidir.ShortName(projDir))
	// The marker is a directory: marker reads fail as "not owned", the marker
	// write fails with EISDIR, and the rest of the directory stays writable, so a
	// render would land there.
	if err := os.MkdirAll(filepath.Join(out, wikidir.MarkerFile), 0o755); err != nil {
		t.Fatalf("create the marker as a directory: %v", err)
	}

	logged := los04gCaptureLog(t)
	runWikiOnce(context.Background(), s, outRoot, codescan.NewCachedScanner(scanDouble{}))

	if got := logged.String(); !strings.Contains(got, "wiki-tick: mark") {
		t.Fatalf("the failed marker write was not logged: %q", got)
	}
	if _, err := os.Stat(filepath.Join(out, "index.md")); err == nil {
		t.Fatalf("the tick rendered into a directory whose marker it could not write: %s", out)
	}
}

// The other side of the guard: a failed write must not freeze a directory that
// provably belongs to this project. Its marker already names it, so rendering
// there is safe even when the marker itself cannot be rewritten (YM-366).
func TestRunWikiOnce_RendersWhenTheMarkerAlreadyNamesTheProject(t *testing.T) {
	if os.Geteuid() == 0 {
		// Root writes a 0444 file anyway, so the marker write would not fail. A
		// root-only CI therefore does not exercise the allow branch of the guard.
		t.Skip("root writes a 0444 file anyway, so the marker write would not fail")
	}
	s := newWikiTickTestStore(t)
	projDir := fixtureProjectDir(t, "los04h/marker-readonly")
	insertLearningForProject(t, s, projDir)
	insertSessionForProject(t, s, projDir, time.Now().Format(time.RFC3339))

	outRoot := t.TempDir()
	out := filepath.Join(outRoot, wikidir.ShortName(projDir))
	if err := wikidir.WriteMarker(out, projDir); err != nil {
		t.Fatalf("write the marker: %v", err)
	}
	marker := filepath.Join(out, wikidir.MarkerFile)
	if err := os.Chmod(marker, 0o444); err != nil {
		t.Fatalf("make the marker read-only: %v", err)
	}

	logged := los04gCaptureLog(t)
	runWikiOnce(context.Background(), s, outRoot, codescan.NewCachedScanner(scanDouble{}))

	if _, err := os.Stat(filepath.Join(out, "index.md")); err != nil {
		t.Fatalf("a project kept its own directory out of the render: %v", err)
	}
	// The marker write has to have failed, or the guard was never reached and the
	// render above proves nothing about the allow branch.
	if got := logged.String(); !strings.Contains(got, "wiki-tick: mark") {
		t.Fatalf("the marker write did not fail, so the allow branch was not tested: %q", got)
	}
}

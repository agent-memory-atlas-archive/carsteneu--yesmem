package briefing

import (
	"errors"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/storage"
)

type los40FailingScan struct{ err error }

func (f los40FailingScan) Scan(rootDir string) (*codescan.ScanResult, error) { return nil, f.err }

func los40CodeMapGenerator(t *testing.T) *Generator {
	t.Helper()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return New(store, 3)
}

// YM-201: the scan error used to be discarded, so the diagnosis from YM-4
// (stderr reason, "not indexed") never reached the briefing.
func TestBuildCodeMap_SurfacesScanError(t *testing.T) {
	g := los40CodeMapGenerator(t)
	g.codeScanner = codescan.NewCachedScanner(los40FailingScan{
		err: errors.New("cbm cli: exit status 1: project home-x not indexed\nno CBM database for /home/x"),
	})

	text := g.buildCodeMap(t.TempDir(), "proj")

	if !strings.Contains(text, "Code map unavailable") {
		t.Fatalf("scan error not surfaced, got %q", text)
	}
	if !strings.Contains(text, "not indexed") {
		t.Errorf("diagnosis lost, got %q", text)
	}
	if strings.Count(text, "\n") > 1 {
		t.Errorf("diagnostic must stay on one line, got %q", text)
	}
}

// A successful scan must not carry the warning line.
func TestBuildCodeMap_NoWarningOnSuccess(t *testing.T) {
	g := los40CodeMapGenerator(t)
	g.codeScanner = codescan.NewCachedScanner(&codescan.DirectoryScanner{})

	text := g.buildCodeMap(t.TempDir(), "proj")
	if strings.Contains(text, "Code map unavailable") {
		t.Errorf("unexpected warning on successful scan: %q", text)
	}
}

// The diagnostic reaches the system block of the outgoing request: control
// characters from scanner stderr must not survive.
func TestCodeMapFailureNote_StripsControlChars(t *testing.T) {
	note := codeMapFailureNote(errors.New("boom \x1b[31mred\x00\nnext line"))

	if strings.ContainsAny(note, "\x1b\x00\n") {
		t.Errorf("control characters survived: %q", note)
	}
	if !strings.Contains(note, "boom") || !strings.Contains(note, "next line") {
		t.Errorf("diagnosis lost: %q", note)
	}
}

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los63bGitCheckout builds a checkout whose HEAD can be read the way the
// scanner reads it: a git directory with a loose ref.
func los63bGitCheckout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "heads"), 0o755); err != nil {
		t.Fatalf("mkdir git data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "refs", "heads", "main"), []byte("los63bhead\n"), 0o644); err != nil {
		t.Fatalf("write ref: %v", err)
	}
	return dir
}

type los63bFailingScanner struct{}

func (los63bFailingScanner) Scan(string) (*codescan.ScanResult, error) {
	return nil, errors.New("cbm cli: exit status 1")
}

type los63bStubScanner struct{ root string }

func (s los63bStubScanner) Scan(string) (*codescan.ScanResult, error) {
	return &codescan.ScanResult{
		RootDir: s.root,
		Files:   []codescan.FileInfo{{Path: "main.go", Language: "go"}},
	}, nil
}

// YM-453: a failed scan used to leave the caller with a wiki that silently
// lacked its code map. It is reported now, and the render still runs from what
// the scan produced (nothing, in this case).
func TestWikiRenderScan_FailedScanIsReported(t *testing.T) {
	dir := los63bGitCheckout(t)
	store, err := storage.Open(filepath.Join(t.TempDir(), "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	graph, scanJSON, scanErr, saveErr := scanForWikiRender(store, dir, los63bFailingScanner{})
	if scanErr == nil {
		t.Fatal("a failed scan must be reported")
	}
	if !strings.Contains(scanErr.Error(), "cbm cli") {
		t.Errorf("the scan error must survive the wrap, got: %v", scanErr)
	}
	if saveErr != nil {
		t.Errorf("a failed scan must not be reported as a failed save: %v", saveErr)
	}
	if graph != nil || scanJSON != "" {
		t.Errorf("nothing may be rendered from a failed scan: graph=%v scanJSON=%q", graph, scanJSON)
	}
	row, err := store.GetProjectScan(codescan.ScanCacheKey(dir))
	if err != nil {
		t.Fatalf("read scan row: %v", err)
	}
	if row != nil {
		t.Errorf("a failed scan must not be stored, got %q", row.ScanJSON)
	}
}

// The successful case keeps the contract the render depends on: the document is
// stored under the checkout's own key, with the head read at that same key.
func TestWikiRenderScan_StoresTheCheckoutsDocument(t *testing.T) {
	dir := los63bGitCheckout(t)
	store, err := storage.Open(filepath.Join(t.TempDir(), "yesmem.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	graph, scanJSON, scanErr, saveErr := scanForWikiRender(store, dir, los63bStubScanner{root: dir})
	if scanErr != nil || saveErr != nil {
		t.Fatalf("a clean scan reported err=%v/%v", scanErr, saveErr)
	}
	if graph == nil || scanJSON == "" {
		t.Fatal("the render needs the graph and the document")
	}
	row, err := store.GetProjectScan(codescan.ScanCacheKey(dir))
	if err != nil || row == nil {
		t.Fatalf("the scan must be stored under the checkout key: row=%v err=%v", row, err)
	}
	if row.ScanJSON != scanJSON {
		t.Error("the stored document differs from the one handed to the render")
	}
	if row.GitHead != "los63bhead" {
		t.Errorf("stored git head = %q, want the head read at the key", row.GitHead)
	}
}

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-261 (Los 11d): Handler.OnMutation is declared as the post-mutation side
// effect but nothing ever assigns it, so every `go h.onMutation()` call in the
// resolve/commit/cap paths was a no-op and MEMORY.md never followed a
// resolution. The hook is wired in the production assembly (not in NewHandler,
// which tests use) so this test drives that seam with a redirected HOME.
func TestWireMutationHook_Los11d_RegeneratesMemoryMD(t *testing.T) {
	home := t.TempDir()
	isolateHomeAt(t, home)

	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	s := store
	project := filepath.Join(home, "projects", "los11d")
	if err := s.UpsertSession(&models.Session{
		ID: "ses-los11d", Project: project, StartedAt: time.Now(), IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	// GenerateAllMemoryMDs only writes for projects Claude Code knows. The
	// directory name comes from the encoder itself: a hand-rolled
	// ReplaceAll(project, "/", "-") would keep the underscores of the t.TempDir()
	// path that Claude Code maps to dashes, and the sweep would find nothing.
	projDir := filepath.Join(home, ".claude", "projects", archive.ProjectDirName(project))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	if NewHandler(s, nil).OnMutation != nil {
		t.Fatal("NewHandler must stay inert: tests would write into the real home")
	}
	// "" keeps the home default, which this test redirects via $HOME.
	h := newProductionHandler(s, nil, "")

	if h.OnMutation == nil {
		t.Fatal("OnMutation is still nil: every post-mutation hook call stays a no-op")
	}
	h.onMutation()

	// The hook sweeps in its own goroutine; give it a moment instead of racing it.
	memFile := filepath.Join(projDir, "memory", "MEMORY.md")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(memFile)
		if err == nil {
			if !strings.Contains(string(data), yesmemMarker) {
				t.Fatalf("MEMORY.md missing the yesmem marker: %q", string(data))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected MEMORY.md after a mutation: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A regenerated MEMORY.md must not be silently widened: a file the user
// tightened keeps its mode through the temp-file rename.
func TestWriteMemoryMD_Los11d_PreservesTheFileMode(t *testing.T) {
	dir := t.TempDir()
	memFile := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(memFile, []byte("manual notes\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := writeMemoryMD(dir, memFile, "\nnarrative\n"); err != nil {
		t.Fatalf("writeMemoryMD: %v", err)
	}

	info, err := os.Stat(memFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode widened to %o", got)
	}
	if data, _ := os.ReadFile(memFile); !strings.Contains(string(data), "manual notes") {
		t.Errorf("manual prefix was lost: %q", string(data))
	}
}

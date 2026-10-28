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

// YM-354 (Los 11j): the sweep built the Claude projects directory by hand from
// $HOME instead of reading config.Paths.ClaudeProjects. A configuration that
// points elsewhere (an alternate CLAUDE_CONFIG_DIR, a test home) was ignored, so
// the run refreshed a folder nobody reads and reported a clean sweep.
func TestGenerateAllMemoryMDs_Los11j_UsesTheConfiguredProjectsDir(t *testing.T) {
	store, home, project := los11jMemoryStore(t)
	configured := t.TempDir()
	projDir := filepath.Join(configured, archive.ProjectDirName(project))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir configured project dir: %v", err)
	}

	GenerateAllMemoryMDs(store, configured)

	los11jAssertMemoryMD(t, filepath.Join(projDir, "memory", "MEMORY.md"))

	// The home-derived path must stay untouched: it is the one this sweep used to
	// write into regardless of the configuration.
	homePath := filepath.Join(home, ".claude", "projects", archive.ProjectDirName(project))
	if _, err := os.Stat(homePath); !os.IsNotExist(err) {
		t.Errorf("the sweep still created %s (stat error %v)", homePath, err)
	}
}

// An empty value is not an error: it keeps the historical default, which is what
// config.Default() produces when no config file names the directory.
func TestGenerateAllMemoryMDs_Los11j_FallsBackToTheHomeDefault(t *testing.T) {
	store, home, project := los11jMemoryStore(t)
	projDir := filepath.Join(home, ".claude", "projects", archive.ProjectDirName(project))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir home project dir: %v", err)
	}

	GenerateAllMemoryMDs(store, "")

	los11jAssertMemoryMD(t, filepath.Join(projDir, "memory", "MEMORY.md"))
}

// The directory has to survive the wiring as well: newProductionHandler hands it
// to the mutation hook, and a dropped parameter there would leave the sweep on
// the home fallback while every direct call looks correct.
func TestWireMutationHook_Los11j_CarriesTheConfiguredProjectsDir(t *testing.T) {
	store, home, project := los11jMemoryStore(t)
	configured := t.TempDir()
	projDir := filepath.Join(configured, archive.ProjectDirName(project))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir configured project dir: %v", err)
	}

	h := newProductionHandler(store, nil, configured)
	if h.OnMutation == nil {
		t.Fatal("OnMutation is nil: the sweep stays a no-op")
	}
	h.onMutation()

	// The hook sweeps in its own goroutine.
	memFile := filepath.Join(projDir, "memory", "MEMORY.md")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(memFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("MEMORY.md missing at %s: the hook did not carry the configured directory", memFile)
		}
		time.Sleep(10 * time.Millisecond)
	}

	homePath := filepath.Join(home, ".claude", "projects", archive.ProjectDirName(project))
	if _, err := os.Stat(homePath); !os.IsNotExist(err) {
		t.Errorf("the hook wrote under $HOME again: %v", err)
	}
}

// Without HOME the sweep has nowhere to write. The earlier code would have joined
// an empty home into ".claude/projects", a path relative to the working
// directory, and created it there.
func TestGenerateAllMemoryMDs_Los11j_WithoutHomeWritesNothing(t *testing.T) {
	t.Setenv("HOME", "")

	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.UpsertSession(&models.Session{
		ID: "ses-los11j-nohome", Project: "/home/someone/work/los11j",
		StartedAt: time.Now(), IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	work := t.TempDir()
	t.Chdir(work)

	GenerateAllMemoryMDs(store, "")

	if _, err := os.Stat(filepath.Join(work, ".claude")); !os.IsNotExist(err) {
		t.Errorf("the sweep created .claude below the working directory: %v", err)
	}
}

// los11jMemoryStore builds a store with one project and redirects $HOME, so a
// sweep that still derives its path from the home directory is visible instead of
// touching the real one.
func los11jMemoryStore(t *testing.T) (*storage.Store, string, string) {
	t.Helper()
	home := t.TempDir()
	isolateHomeAt(t, home)

	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	const project = "/home/someone/work/los11j"
	if err := store.UpsertSession(&models.Session{
		ID: "ses-los11j-mem", Project: project, StartedAt: time.Now(), IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	return store, home, project
}

func los11jAssertMemoryMD(t *testing.T, memFile string) {
	t.Helper()
	data, err := os.ReadFile(memFile)
	if err != nil {
		t.Fatalf("MEMORY.md missing at %s: %v", memFile, err)
	}
	if !strings.Contains(string(data), yesmemMarker) {
		t.Errorf("MEMORY.md without the yesmem marker: %q", string(data))
	}
}

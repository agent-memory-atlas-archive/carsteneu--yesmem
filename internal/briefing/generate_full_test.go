package briefing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// The project key used by the follow-up lookups in GenerateFullBriefing (refine
// cache, pins, unfinished count) must match the key the rest of the system uses:
// sessions.project_short, learnings.project and refined_briefings.project all
// store the absolute project path, never a basename.

func TestGenerateFullBriefing_PinScopedToProjectPath(t *testing.T) {
	store := setupStore(t)
	project := t.TempDir()

	if _, err := store.PinLearning("permanent", project, "PIN-ABSPATH-MARKER", "test"); err != nil {
		t.Fatalf("PinLearning: %v", err)
	}

	res := GenerateFullBriefing(store, t.TempDir(), project, "")
	if !strings.Contains(res.Text, "PIN-ABSPATH-MARKER") {
		t.Errorf("pin scoped to %q missing from that project's briefing", project)
	}
}

func TestGenerateFullBriefing_GlobalPinStillVisible(t *testing.T) {
	store := setupStore(t)
	project := t.TempDir()

	if _, err := store.PinLearning("permanent", "", "GLOBAL-PIN-MARKER", "test"); err != nil {
		t.Fatalf("PinLearning: %v", err)
	}

	res := GenerateFullBriefing(store, t.TempDir(), project, "")
	if !strings.Contains(res.Text, "GLOBAL-PIN-MARKER") {
		t.Error("global pin missing from briefing")
	}
}

func TestGenerateFullBriefing_RefineCacheKeyedByProjectPath(t *testing.T) {
	store := setupStore(t)
	project := t.TempDir()

	if err := store.SaveRefinedBriefing(project, "hash", "VERFEINERT-MARKER", "test"); err != nil {
		t.Fatalf("SaveRefinedBriefing: %v", err)
	}

	res := GenerateFullBriefing(store, t.TempDir(), project, "")
	if !strings.Contains(res.Text, "VERFEINERT-MARKER") {
		t.Error("cached refined briefing stored under the project path was not used")
	}
}

func TestGenerateFullBriefing_OpenWorkCountsProjectScopedItems(t *testing.T) {
	store := setupStore(t)
	project := t.TempDir()
	dataDir := t.TempDir()

	writeDataFile(t, filepath.Join(dataDir, "config.yaml"), "briefing:\n  remind_open_work: true\n")
	writeDataFile(t, filepath.Join(dataDir, "strings.yaml"), "open_work_remind: 'OFFENE-ARBEIT: %s'\n")

	if _, err := store.InsertLearning(&models.Learning{
		Category:  "unfinished",
		Content:   "AUFGABE-X",
		Project:   project,
		TaskType:  "task",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}

	res := GenerateFullBriefing(store, dataDir, project, "")
	if !strings.Contains(res.Text, "OFFENE-ARBEIT") {
		t.Errorf("unfinished learning scoped to %q not counted for the open-work reminder", project)
	}
}

func writeDataFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

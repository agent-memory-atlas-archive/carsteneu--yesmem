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

	if _, err := store.PinLearning("permanent", project, "PIN-ABSPATH-MARKER", "test", ""); err != nil {
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

	if _, err := store.PinLearning("permanent", "", "GLOBAL-PIN-MARKER", "test", ""); err != nil {
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

// YM-105: the delivery path must apply the detected source agent, otherwise a
// non-Claude session keeps reading Claude-specific wording ("Claude: Bring up
// these points"). The session id arrives from the proxy/assemble callers.
func TestGenerateFullBriefing_NonClaudeWordingFromSourceAgent(t *testing.T) {
	store := setupStore(t)
	project := t.TempDir()
	dataDir := t.TempDir()

	if _, err := store.InsertLearning(&models.Learning{
		Category:  "unfinished",
		Content:   "AUFGABE-Y",
		Project:   project,
		TaskType:  "task",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertLearning: %v", err)
	}
	if err := store.SetProxyState("source_agent:opencode:ses-1", "opencode"); err != nil {
		t.Fatalf("SetProxyState: %v", err)
	}

	res := GenerateFullBriefing(store, dataDir, project, "opencode:ses-1")
	if !strings.Contains(res.Text, "AUFGABE-Y") {
		t.Fatalf("open-work item missing, cannot judge wording:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "Claude: Bring up these points") {
		t.Errorf("non-Claude session still gets Claude-specific wording:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "Agent: Bring up these points") {
		t.Errorf("expected neutral agent wording:\n%s", res.Text)
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

// YM-162: the recovery handoff must not be a race against a wall clock. A user
// who clears the conversation and types the next prompt two minutes later still
// gets the previous session's context, and the handoff belongs to that first
// briefing only, so a second briefing does not repeat it.
func TestGenerateFullBriefing_RecoversFromTrackedClearWithoutTimeWindow(t *testing.T) {
	store := setupStore(t)
	dataDir := t.TempDir()
	const project = "/var/www/myproject"

	const previous = "recovered-session"
	if err := store.UpsertSession(&models.Session{
		ID: previous, Project: project, ProjectShort: project,
		FirstMessage: "Fix the login bug", MessageCount: 1,
		JSONLPath: "/recovered.jsonl", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := store.InsertMessages([]models.Message{
		{SessionID: previous, Role: "user", MessageType: "text", Content: "Fix the login bug in auth.go", Sequence: 0},
	}); err != nil {
		t.Fatalf("InsertMessages: %v", err)
	}
	// A tracked clear from two minutes ago: well outside the former 30s window.
	if _, err := store.DB().Exec(
		`INSERT INTO session_tracking (project, session_id, reason, timestamp) VALUES (?, ?, ?, ?)`,
		project, previous, "clear", time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed session_tracking: %v", err)
	}

	res := GenerateFullBriefing(store, dataDir, project, "")
	if !strings.Contains(res.Text, "vor Clear") {
		t.Fatal("first briefing after a tracked clear carries no recovery block")
	}
	if !strings.Contains(res.Text, previous) {
		t.Errorf("recovery block does not reference the ended session %q", previous)
	}

	again := GenerateFullBriefing(store, dataDir, project, "")
	if strings.Contains(again.Text, "vor Clear") {
		t.Error("the recovery handoff was injected twice: it must be consumed by the first briefing")
	}
}

// A tracked clear whose follow-up session never starts must not surface as fresh
// context days later.
func TestGenerateFullBriefing_DropsStaleTrackedClear(t *testing.T) {
	store := setupStore(t)
	dataDir := t.TempDir()
	const project = "/var/www/myproject"

	if _, err := store.DB().Exec(
		`INSERT INTO session_tracking (project, session_id, reason, timestamp) VALUES (?, ?, ?, ?)`,
		project, "s1", "clear", time.Now().Add(-30*24*time.Hour).UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed session_tracking: %v", err)
	}

	res := GenerateFullBriefing(store, dataDir, project, "")
	if strings.Contains(res.Text, "vor Clear") {
		t.Error("a month-old tracked clear was injected as a recovery block")
	}
}

func writeDataFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

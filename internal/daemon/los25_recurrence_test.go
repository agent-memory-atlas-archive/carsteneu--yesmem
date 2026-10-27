package daemon

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

func los25SeedRecurrenceCluster(t *testing.T, s interface {
	InsertLearning(*models.Learning) (int64, error)
	ReplaceLearningClusters(string, []models.LearningCluster) error
}, project string, learnings int) []int64 {
	t.Helper()
	ids := make([]int64, 0, learnings)
	base := time.Now().Add(-48 * time.Hour)
	for i := 0; i < learnings; i++ {
		id, err := s.InsertLearning(&models.Learning{
			Category:   "gotcha",
			Content:    fmt.Sprintf("los25 recurrence input %d", i),
			Project:    project,
			Source:     "llm_extracted",
			Confidence: 1.0,
			CreatedAt:  base.Add(time.Duration(i) * time.Minute),
			SessionID:  fmt.Sprintf("los25rec-sess-%d", i%2),
		})
		if err != nil {
			t.Fatalf("insert recurrence input %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		t.Fatalf("marshal ids: %v", err)
	}
	if err := s.ReplaceLearningClusters(project, []models.LearningCluster{{
		Label:          "los25 recurring topic",
		LearningCount:  learnings,
		AvgRecencyDays: 1,
		Confidence:     0.9,
		LearningIDs:    string(raw),
	}}); err != nil {
		t.Fatalf("seed clusters: %v", err)
	}
	return ids
}

// YM-116: recurrence alerts were inserted without created_at, so the briefing's
// "latest three alerts" had no order to sort by.
func TestLos25_RecurrenceAlertStampsCreatedAt(t *testing.T) {
	s := mustStore(t)
	const project = "/home/testuser/projects/los25-recurrence"

	los25SeedRecurrenceCluster(t, s, project, 3)

	alerts := detectForProject(s, &los25CountingLLM{reply: "architecture problem"}, project, project)
	if alerts != 1 {
		t.Fatalf("alerts = %d, want 1", alerts)
	}

	stored, err := s.GetLearningsByCategory("recurrence_alert", project, 5)
	if err != nil {
		t.Fatalf("get recurrence alerts: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored alerts = %d, want 1", len(stored))
	}
	if stored[0].CreatedAt.IsZero() {
		t.Fatalf("recurrence alert has no created_at")
	}
}

// YM-122: the extraction phase ran recurrence on every batch with no volume gate,
// so the LLM was paid for clusters that could not have changed.
func TestLos25_RecurrenceSkipsBelowTheLearningGate(t *testing.T) {
	s := mustStore(t)
	const project = "/home/testuser/projects/los25-gate"

	los25UpsertSession(t, s, "los25-gate-sess", project)
	los25SeedRecurrenceCluster(t, s, project, 3)

	client := &los25CountingLLM{reply: "architecture problem"}
	if n := DetectRecurrence(s, client); n != 0 {
		t.Fatalf("alerts = %d, want 0 below the new-learning gate", n)
	}
	if got := client.CallCount(); got != 0 {
		t.Fatalf("LLM calls = %d, want 0 below the gate", got)
	}
}

// YM-122: above the gate the detector runs and records a cursor, so the next call
// with no new learnings is free.
func TestLos25_RecurrenceRunsAboveTheGateAndAdvancesTheCursor(t *testing.T) {
	s := mustStore(t)
	const project = "/home/testuser/projects/los25-gate-above"

	los25UpsertSession(t, s, "los25-gate-above-sess", project)
	los25SeedRecurrenceCluster(t, s, project, recurrenceMinNewLearnings)

	client := &los25CountingLLM{reply: "architecture problem"}
	if n := DetectRecurrence(s, client); n != 1 {
		t.Fatalf("alerts = %d, want 1 above the gate", n)
	}
	calls := client.CallCount()
	if calls == 0 {
		t.Fatalf("LLM was never asked above the gate")
	}

	cursor, _ := s.GetProxyState(recurrenceCursorKey)
	if cursor == "" {
		t.Fatalf("no recurrence cursor stored after the run")
	}

	if n := DetectRecurrence(s, client); n != 0 {
		t.Fatalf("second run alerts = %d, want 0 (no new learnings)", n)
	}
	if got := client.CallCount(); got != calls {
		t.Fatalf("second run made %d extra LLM calls, want 0", got-calls)
	}
}

// YM-122 review round 1: a cursor above the learning table (restore, manual
// edit) would silence detection forever. It must reset instead.
func TestLos25_RecurrenceResetsImpossibleCursor(t *testing.T) {
	s := mustStore(t)
	const project = "/home/testuser/projects/los25-gate-cursor"

	los25UpsertSession(t, s, "los25-gate-cursor-sess", project)
	los25SeedRecurrenceCluster(t, s, project, recurrenceMinNewLearnings)
	if err := s.SetProxyState(recurrenceCursorKey, "999999999"); err != nil {
		t.Fatalf("set cursor: %v", err)
	}

	client := &los25CountingLLM{reply: "architecture problem"}
	if n := DetectRecurrence(s, client); n != 1 {
		t.Fatalf("alerts = %d, want 1 after resetting an impossible cursor", n)
	}
}

// YM-122 review round 1: verdict keys belong to the current cluster membership.
// When membership changes, the old key must not stay in proxy_state forever.
func TestLos25_RecurrencePrunesStaleVerdicts(t *testing.T) {
	s := mustStore(t)
	const project = "/home/testuser/projects/los25-verdict-prune"

	ids := los25SeedRecurrenceCluster(t, s, project, 3)
	client := &los25CountingLLM{reply: "kein_problem"}
	if n := detectForProject(s, client, project, project); n != 0 {
		t.Fatalf("alerts = %d, want 0", n)
	}

	staleKey := "recurrence_verdict:" + project + ":" + recurrenceClusterKey(ids)
	if verdict, _ := s.GetProxyState(staleKey); verdict != "kein_problem" {
		t.Fatalf("verdict not stored, got %q", verdict)
	}

	// Rotate the membership: the old verdict no longer describes the cluster.
	for _, id := range ids {
		if err := s.SupersedeLearning(id, 0, "los25 test: rotate cluster"); err != nil {
			t.Fatalf("supersede %d: %v", id, err)
		}
	}
	los25SeedRecurrenceCluster(t, s, project, 3)
	if n := detectForProject(s, client, project, project); n != 0 {
		t.Fatalf("alerts = %d, want 0", n)
	}

	if verdict, _ := s.GetProxyState(staleKey); verdict != "" {
		t.Fatalf("stale verdict %q survived the membership change", staleKey)
	}
}

// YM-122: a "kein_problem" verdict was thrown away, so every batch paid again for
// the same cluster. The verdict must be remembered per cluster, and a different
// cluster must still be evaluated.
func TestLos25_RecurrenceRemembersKeinProblemVerdict(t *testing.T) {
	s := mustStore(t)
	const project = "/home/testuser/projects/los25-verdict"

	first := los25SeedRecurrenceCluster(t, s, project, 3)
	client := &los25CountingLLM{reply: "kein_problem"}

	if n := detectForProject(s, client, project, project); n != 0 {
		t.Fatalf("alerts = %d, want 0 for kein_problem", n)
	}
	if got := client.CallCount(); got != 1 {
		t.Fatalf("first run LLM calls = %d, want 1", got)
	}

	if n := detectForProject(s, client, project, project); n != 0 {
		t.Fatalf("second run alerts = %d, want 0", n)
	}
	if got := client.CallCount(); got != 1 {
		t.Fatalf("second run asked the LLM again (calls = %d), want the verdict remembered", got)
	}

	// A different cluster carries a different verdict key and must be evaluated.
	for _, id := range first {
		if err := s.SupersedeLearning(id, 0, "los25 test: rotate cluster"); err != nil {
			t.Fatalf("supersede %d: %v", id, err)
		}
	}
	los25SeedRecurrenceCluster(t, s, project, 3)
	if n := detectForProject(s, client, project, project); n != 0 {
		t.Fatalf("alerts = %d, want 0", n)
	}
	if got := client.CallCount(); got != 2 {
		t.Fatalf("a changed cluster was not evaluated (calls = %d, want 2)", got)
	}
}

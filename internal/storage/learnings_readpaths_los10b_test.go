package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// insertReadPathLos10b inserts a learning with the fields the given read path
// needs and returns its ID.
func insertReadPathLos10b(t *testing.T, s *Store, l *models.Learning) int64 {
	t.Helper()
	if l.Project == "" {
		l.Project = "los10b-proj"
	}
	if l.Confidence == 0 {
		l.Confidence = 1.0
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	if l.ModelUsed == "" {
		l.ModelUsed = "test"
	}
	if l.Source == "" {
		l.Source = "llm_extracted"
	}
	id, err := s.InsertLearning(l)
	if err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	return id
}

// TestPromptReadPathsExcludeQuarantinedLos10b (YM-245 [MEM-F28]): four more read
// paths feed the prompt or the consolidation input and still selected
// quarantined rows: deadline/trigger rules, session narratives, pulse rows and
// the consolidation window.
func TestPromptReadPathsExcludeQuarantinedLos10b(t *testing.T) {
	s := newTestStore(t)
	const project = "los10b-proj"
	const session = "sess-los10b-readpaths"

	triggerID := insertReadPathLos10b(t, s, &models.Learning{
		SessionID: session, Category: "gotcha", Content: "quarantined deadline",
		TriggerRule: "deadline:2026-12-31",
	})
	narrativeID := insertReadPathLos10b(t, s, &models.Learning{
		SessionID: session, Category: "narrative", Content: "quarantined narrative",
	})
	pulseID := insertReadPathLos10b(t, s, &models.Learning{
		SessionID: session, Category: "pulse", Content: "quarantined pulse",
	})
	consolidationID := insertReadPathLos10b(t, s, &models.Learning{
		SessionID: session, Category: "pattern", Content: "quarantined pattern",
	})
	activeID := insertReadPathLos10b(t, s, &models.Learning{
		SessionID: "sess-los10b-readpaths-active", Category: "pattern", Content: "active pattern",
	})

	if _, err := s.QuarantineSession(session); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	triggered, err := s.GetTriggeredLearnings(project, "deadline:", "")
	if err != nil {
		t.Fatalf("get triggered learnings: %v", err)
	}
	for _, l := range triggered {
		if l.ID == triggerID {
			t.Errorf("quarantined trigger learning %d reached the deadline path", triggerID)
		}
	}

	narratives, err := s.GetRecentNarratives(project, 5)
	if err != nil {
		t.Fatalf("get recent narratives: %v", err)
	}
	for _, l := range narratives {
		if l.ID == narrativeID {
			t.Errorf("quarantined narrative %d reached the briefing", narrativeID)
		}
	}

	pulses, err := s.GetPulseLearningsSince(project, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("get pulse learnings: %v", err)
	}
	for _, l := range pulses {
		if l.ID == pulseID {
			t.Errorf("quarantined pulse learning %d reached the pulse path", pulseID)
		}
	}

	consolidated, err := s.GetConsolidationLearnings(0, consolidationID+1)
	if err != nil {
		t.Fatalf("get consolidation learnings: %v", err)
	}
	seenActive := false
	for _, l := range consolidated {
		if l.ID == consolidationID {
			t.Errorf("quarantined learning %d entered the consolidation window", consolidationID)
		}
		if l.ID == activeID {
			seenActive = true
		}
	}
	if !seenActive {
		t.Errorf("active learning %d missing from the consolidation window", activeID)
	}
}

// TestRecentLearningIDsSkipsQuarantinedLos10b (YM-246 [MEM-F29]): the skip_recent
// window took the newest IDs regardless of state, so a quarantined row consumed
// a slot and the protection silently covered one row fewer.
func TestRecentLearningIDsSkipsQuarantinedLos10b(t *testing.T) {
	s := newTestStore(t)

	olderID := insertReadPathLos10b(t, s, &models.Learning{
		SessionID: "sess-los10b-skip-old", Category: "gotcha", Content: "older skip candidate",
	})
	newestID := insertReadPathLos10b(t, s, &models.Learning{
		SessionID: "sess-los10b-skip-new", Category: "gotcha", Content: "newest skip candidate",
	})
	if _, err := s.QuarantineSession("sess-los10b-skip-new"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	ids, err := s.GetRecentLearningIDs(1, "")
	if err != nil {
		t.Fatalf("recent ids: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("got %d ids, want 1", len(ids))
	}
	if ids[0] == newestID {
		t.Errorf("quarantined learning %d consumed the skip slot", newestID)
	}
	if ids[0] != olderID {
		t.Errorf("skip slot went to %d, want the recallable %d", ids[0], olderID)
	}
}

// TestStorageRejectsInvalidBoundsLos10b (YM-246 [MEM-F29]): the storage layer
// owns the SQL comparison, so it must not turn an unreadable bound into "no
// filter" on its own. Before, an unparsable bound was compared as text.
func TestStorageRejectsInvalidBoundsLos10b(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.GetActiveLearnings("gotcha", "los10b-proj", "gestern", "", 0); err == nil {
		t.Error("GetActiveLearnings accepted since=gestern")
	}
	if _, err := s.SearchLearningsBM25Ctx(t.Context(), "los10b bound probe", "", "", "last week", 5); err == nil {
		t.Error("SearchLearningsBM25Ctx accepted before=last week")
	}
	if _, err := s.SearchAnticipatedQueries("los10b bound probe", "", 5, "irgendwann", ""); err == nil {
		t.Error("SearchAnticipatedQueries accepted since=irgendwann")
	}

	// Control: the documented forms pass.
	if _, err := s.GetActiveLearnings("gotcha", "los10b-proj", "2026-07-01", "", 5); err != nil {
		t.Errorf("GetActiveLearnings rejected the documented bound: %v", err)
	}
}

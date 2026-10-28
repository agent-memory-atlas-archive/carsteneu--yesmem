package storage

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// insertQuarantineLos10bLearning inserts a learning with its own session and
// project so a later QuarantineSession is unambiguous.
func insertQuarantineLos10bLearning(t *testing.T, s *Store, content, sessionID, project, flavor string) int64 {
	t.Helper()
	id, err := s.InsertLearning(&models.Learning{
		SessionID:     sessionID,
		Category:      "gotcha",
		Content:       content,
		Project:       project,
		Confidence:    1.0,
		CreatedAt:     time.Now().Add(-10 * time.Minute),
		ModelUsed:     "test",
		Source:        "llm_extracted",
		SessionFlavor: flavor,
	})
	if err != nil {
		t.Fatalf("insert learning %q: %v", content, err)
	}
	return id
}

// TestGetActiveLearningsExcludesQuarantinedLos10b (YM-245 [MEM-F28]): the
// get_learnings path, the CLAUDE.md/AGENTS.md generator and the briefing all
// read through GetActiveLearnings, which filtered only superseded_by. A
// quarantined session's learnings were recalled everywhere.
func TestGetActiveLearningsExcludesQuarantinedLos10b(t *testing.T) {
	s := mustOpen(t)

	const project = "los10b-proj"
	activeID := insertQuarantineLos10bLearning(t, s, "active project rule", "sess-los10b-active", project, "")
	quarantinedID := insertQuarantineLos10bLearning(t, s, "broken session guess", "sess-los10b-quar", project, "")

	if _, err := s.QuarantineSession("sess-los10b-quar"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	learnings, err := s.GetActiveLearnings("gotcha", project, "", "", 0)
	if err != nil {
		t.Fatalf("get active learnings: %v", err)
	}

	got := make(map[int64]bool, len(learnings))
	for _, l := range learnings {
		got[l.ID] = true
	}
	if !got[activeID] {
		t.Errorf("active learning %d missing from GetActiveLearnings", activeID)
	}
	if got[quarantinedID] {
		t.Errorf("quarantined learning %d was returned by GetActiveLearnings", quarantinedID)
	}
}

// TestGetLearningsSinceExcludesQuarantinedLos10b (YM-245 [MEM-F28]): the
// metamemory path (proxy_stub) injects GetLearningsSince rows straight into the
// prompt; a quarantined learning reached the model unfiltered.
func TestGetLearningsSinceExcludesQuarantinedLos10b(t *testing.T) {
	s := mustOpen(t)

	const project = "los10b-proj"
	activeID := insertQuarantineLos10bLearning(t, s, "active decision", "sess-los10b-active-since", project, "")
	quarantinedID := insertQuarantineLos10bLearning(t, s, "quarantined decision", "sess-los10b-quar-since", project, "")

	if _, err := s.QuarantineSession("sess-los10b-quar-since"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	learnings, err := s.GetLearningsSince(project, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("get learnings since: %v", err)
	}

	got := make(map[int64]bool, len(learnings))
	for _, l := range learnings {
		got[l.ID] = true
	}
	if !got[activeID] {
		t.Errorf("active learning %d missing from GetLearningsSince", activeID)
	}
	if got[quarantinedID] {
		t.Errorf("quarantined learning %d was returned by GetLearningsSince", quarantinedID)
	}
}

// TestGetSessionFlavorsSinceExcludesQuarantinedLos10b (YM-245 [MEM-F28]): the
// session-flavor list is built from learnings rows and is part of the briefing
// input. A quarantined session's flavor text is content and must not surface.
func TestGetSessionFlavorsSinceExcludesQuarantinedLos10b(t *testing.T) {
	s := mustOpen(t)

	const project = "los10b-proj"
	insertQuarantineLos10bLearning(t, s, "active flavor row", "sess-los10b-flavor-active", project, "calm debugging")
	insertQuarantineLos10bLearning(t, s, "quarantined flavor row", "sess-los10b-flavor-quar", project, "panicked guessing")

	if _, err := s.QuarantineSession("sess-los10b-flavor-quar"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	flavors, err := s.GetSessionFlavorsSince(project, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("get session flavors since: %v", err)
	}

	seen := make(map[string]bool, len(flavors))
	for _, f := range flavors {
		if v, ok := f["session_flavor"].(string); ok {
			seen[v] = true
		}
	}
	if !seen["calm debugging"] {
		t.Errorf("active session flavor missing: %v", flavors)
	}
	if seen["panicked guessing"] {
		t.Errorf("quarantined session flavor was returned: %v", flavors)
	}
}

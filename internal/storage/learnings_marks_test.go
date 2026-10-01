package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

func TestCountLearningsForSession(t *testing.T) {
	s := mustOpen(t)

	s.UpsertSession(&models.Session{
		ID: "sess-a", Project: "/test", ProjectShort: "test",
		StartedAt: time.Now(), JSONLPath: "/a.jsonl", IndexedAt: time.Now(),
	})
	s.InsertLearning(&models.Learning{
		Category: "narrative", Content: "n1", SessionID: "sess-a",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})
	s.InsertLearning(&models.Learning{
		Category: "pulse", Content: "p1", SessionID: "sess-a",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})
	s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "g1", SessionID: "sess-a",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})
	s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "other", SessionID: "sess-b",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})

	n, err := s.CountLearningsForSession("sess-a")
	if err != nil {
		t.Fatalf("CountLearningsForSession: %v", err)
	}
	// narrative/pulse are daemon artifacts, not marks: a session whose only
	// output is its own narrative must count 0 (confabulation gate #72444)
	if n != 1 {
		t.Errorf("expected 1 real mark for sess-a (narrative+pulse excluded), got %d", n)
	}

	n, _ = s.CountLearningsForSession("sess-none")
	if n != 0 {
		t.Errorf("expected 0 for unknown session, got %d", n)
	}
}

func TestGetLearningsCounts(t *testing.T) {
	s := mustOpen(t)

	for i, sid := range []string{"sa", "sb", "sc"} {
		s.UpsertSession(&models.Session{
			ID: sid, Project: "/test", ProjectShort: "test",
			StartedAt: time.Now(), JSONLPath: "/x.jsonl", IndexedAt: time.Now(),
		})
		for j := 0; j <= i; j++ {
			s.InsertLearning(&models.Learning{
				Category: "gotcha", Content: "l", SessionID: sid,
				Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
			})
		}
	}

	counts, err := s.GetLearningsCounts([]string{"sa", "sb", "sc", "sx"})
	if err != nil {
		t.Fatalf("GetLearningsCounts: %v", err)
	}
	if counts["sa"] != 1 || counts["sb"] != 2 || counts["sc"] != 3 {
		t.Errorf("expected 1/2/3, got %v", counts)
	}
	if _, ok := counts["sx"]; ok {
		t.Error("unknown session should not appear in counts")
	}
}

func TestGetLatestStateBrief(t *testing.T) {
	s := mustOpen(t)

	mk := func(content string, created time.Time) *models.Learning {
		return &models.Learning{
			Category: "strategic", Content: content, Project: "/home/testuser/memory/yesmem",
			Confidence: 1.0, CreatedAt: created, ModelUsed: "self",
		}
	}

	old := mk("State brief, 2026-09-26 evening: the arc began with a missing briefing.", time.Now().Add(-48*time.Hour))
	if _, err := s.InsertLearning(old); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	newer := mk("State brief, 2026-09-27 evening: the arc is closed.", time.Now().Add(-12*time.Hour))
	if _, err := s.InsertLearning(newer); err != nil {
		t.Fatalf("insert newer: %v", err)
	}
	// Same age as newer but not a brief — must never win by recency
	if _, err := s.InsertLearning(mk("Eager stubbing runs only on the Anthropic path.", time.Now())); err != nil {
		t.Fatalf("insert non-brief: %v", err)
	}

	got, err := s.GetLatestStateBrief("yesmem")
	if err != nil {
		t.Fatalf("GetLatestStateBrief: %v", err)
	}
	if got == nil {
		t.Fatal("expected latest state brief, got none")
	}
	if !strings.Contains(got.Content, "the arc is closed") {
		t.Errorf("expected newest brief, got: %.60s", got.Content)
	}
}

func TestGetLatestStateBrief_LocalWinsOverNewerGlobal(t *testing.T) {
	s := mustOpen(t)

	// Older, but ACTIVE and project-local — must win over the newer foreign brief
	if _, err := s.InsertLearning(&models.Learning{
		Category: "strategic", Content: "State brief, local head", Project: "/home/testuser/memory/yesmem",
		Confidence: 1.0, CreatedAt: time.Now().Add(-24 * time.Hour), ModelUsed: "self",
	}); err != nil {
		t.Fatalf("insert local: %v", err)
	}
	if _, err := s.InsertLearning(&models.Learning{
		Category: "strategic", Content: "State brief, newer global head", Project: "/var/www/green",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	}); err != nil {
		t.Fatalf("insert global: %v", err)
	}

	got, err := s.GetLatestStateBrief("yesmem")
	if err != nil {
		t.Fatalf("GetLatestStateBrief: %v", err)
	}
	if got == nil || !strings.Contains(got.Content, "local head") {
		t.Fatalf("expected project-local brief to win, got %+v", got)
	}
}

func TestGetLatestStateBrief_FallsBackToGlobalWhenNoLocalActive(t *testing.T) {
	s := mustOpen(t)

	// yesmem brief exists but is superseded (e.g. by an old cross-project write)
	localID, err := s.InsertLearning(&models.Learning{
		Category: "strategic", Content: "State brief, dead local head", Project: "/home/testuser/memory/yesmem",
		Confidence: 1.0, CreatedAt: time.Now().Add(-48 * time.Hour), ModelUsed: "self",
	})
	if err != nil {
		t.Fatalf("insert local: %v", err)
	}
	foreignID, err := s.InsertLearning(&models.Learning{
		Category: "strategic", Content: "State brief, active foreign head", Project: "/var/www/green",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})
	if err != nil {
		t.Fatalf("insert foreign: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE learnings SET superseded_by = ? WHERE id = ?`, foreignID, localID); err != nil {
		t.Fatalf("supersede local: %v", err)
	}
	// Even newer, but superseded too — must not win the fallback
	newestID, err := s.InsertLearning(&models.Learning{
		Category: "strategic", Content: "State brief, superseded newest", Project: "/home/testuser/memory/yesmem",
		Confidence: 1.0, CreatedAt: time.Now(), ModelUsed: "self",
	})
	if err != nil {
		t.Fatalf("insert newest: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE learnings SET superseded_by = ? WHERE id = ?`, localID, newestID); err != nil {
		t.Fatalf("supersede newest: %v", err)
	}

	got, err := s.GetLatestStateBrief("yesmem")
	if err != nil {
		t.Fatalf("GetLatestStateBrief: %v", err)
	}
	if got == nil || !strings.Contains(got.Content, "active foreign head") {
		t.Fatalf("expected global fallback brief, got %+v", got)
	}
}

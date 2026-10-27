package briefing

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-104 / YM-108: three call sites build a briefing generator by hand, and the
// background path forgot the config knobs. One constructor reads them all, so a
// knob that is wired once cannot be dropped by a later call site.
func TestLos16NewFromConfigAppliesMaxPerCategory(t *testing.T) {
	s, project := los16BriefingStore(t)
	for _, content := range []string{
		"los16 adamant harbour clause",
		"los16 bitter meadows requiem",
		"los16 crimson valley noise",
	} {
		if _, err := s.InsertLearning(&models.Learning{
			Category: "gotcha", Content: content, Project: project,
			Confidence: 1.0, CreatedAt: time.Now(), Source: "llm_extracted",
		}); err != nil {
			t.Fatalf("insert learning: %v", err)
		}
	}

	cfg := &config.Config{}
	cfg.Briefing.MaxPerCategory = 1
	text := NewFromConfig(s, cfg).Generate(project)

	rendered := 0
	for _, content := range []string{
		"los16 adamant harbour",
		"los16 bitter meadows",
		"los16 crimson valley",
	} {
		if strings.Contains(text, content) {
			rendered++
		}
	}
	if rendered != 1 {
		t.Errorf("briefing carried %d of 3 same-category learnings, want 1: max_per_category was not applied", rendered)
	}
}

func TestLos16NewFromConfigAppliesUnfinishedTTL(t *testing.T) {
	s, project := los16BriefingStore(t)
	for _, content := range []string{
		"los16 stale task alpha",
		"los16 stale task beta",
	} {
		if _, err := s.InsertLearning(&models.Learning{
			Category: "unfinished", TaskType: "task", Content: content,
			Project: project, Confidence: 1.0, Importance: 4,
			CreatedAt: time.Now().Add(-200 * 24 * time.Hour), Source: "llm_extracted",
		}); err != nil {
			t.Fatalf("insert unfinished: %v", err)
		}
	}

	cfg := &config.Config{}
	cfg.Evolution.UnfinishedTTL = 30
	text := NewFromConfig(s, cfg).Generate(project)

	if strings.Contains(text, "los16 stale task") {
		t.Errorf("open work older than the configured unfinished TTL still rendered")
	}
}

func TestLos16NewFromConfigAppliesOtherProjectsDays(t *testing.T) {
	s, project := los16BriefingStore(t)
	seedOtherProject(t, s, "/los16/active-neighbour", 2)
	seedOtherProject(t, s, "/los16/dormant-neighbour", 200)

	cfg := &config.Config{}
	cfg.Briefing.OtherProjectsDays = 90
	text := NewFromConfig(s, cfg).Generate(project)

	if !strings.Contains(text, "/los16/active-neighbour") {
		t.Errorf("recently active neighbour project missing from the briefing")
	}
	if strings.Contains(text, "/los16/dormant-neighbour") {
		t.Errorf("project dormant for 200 days still listed although other_projects_days=90")
	}
}

func TestLos16NewFromConfigAppliesMaxTokens(t *testing.T) {
	s, project := los16BriefingStore(t)
	if err := s.SavePersonaDirective(&models.PersonaDirective{
		UserID: "user_profile", Directive: "x" + strings.Repeat("ä", 30000),
		TraitsHash: "los16", GeneratedAt: time.Now(), ModelUsed: "opus",
	}); err != nil {
		t.Fatalf("save persona directive: %v", err)
	}

	cfg := &config.Config{}
	cfg.Briefing.MaxTokens = 50 // 4 bytes per token
	text := NewFromConfig(s, cfg).Generate(project)

	const suffix = "\n... (briefing truncated)"
	if !strings.HasSuffix(text, suffix) {
		t.Fatalf("briefing was not truncated (%d bytes)", len(text))
	}
	if limit := 50*4 + len(suffix); len(text) > limit {
		t.Errorf("briefing is %d bytes, want at most %d: max_tokens was not applied", len(text), limit)
	}
}

// seedOtherProject creates five sessions for a project path, daysOld days ago.
func seedOtherProject(t *testing.T, s *storage.Store, project string, daysOld int) {
	t.Helper()
	for i := 0; i < 5; i++ {
		if err := s.UpsertSession(&models.Session{
			ID:           fmt.Sprintf("los16-%s-%d", filepath.Base(project), i),
			Project:      project,
			ProjectShort: project,
			FirstMessage: "los16 neighbour fixture",
			MessageCount: 3,
			StartedAt:    time.Now().Add(-time.Duration(daysOld*24+i) * time.Hour),
			JSONLPath:    "/los16-neighbour.jsonl",
			IndexedAt:    time.Now(),
		}); err != nil {
			t.Fatalf("upsert neighbour session: %v", err)
		}
	}
}

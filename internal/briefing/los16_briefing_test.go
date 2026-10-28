package briefing

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los16BriefingStore builds a store with one session for a fresh project
// directory, so Generate resolves the project without touching live data.
func los16BriefingStore(t *testing.T) (*storage.Store, string) {
	t.Helper()
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	project := t.TempDir()
	if err := s.UpsertSession(&models.Session{
		ID: "los16-s1", Project: project, ProjectShort: project,
		FirstMessage: "los16 briefing fixture", MessageCount: 4,
		StartedAt: time.Now().Add(-2 * time.Hour),
		JSONLPath: "/los16-s1.jsonl",
		IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	return s, project
}

// YM-34: the briefing scores learnings with models.ContextualScoreAndSort, but
// nothing filled CurrentTurnCount, so every learning kept full relevance and the
// project boost stayed at 1.5x. With a project that has 200 turns behind it, a
// learning created at turn 0 must lose against one created at turn 190.
func TestLos16BriefingPrefersFreshOverTurnDecayedLearning(t *testing.T) {
	s, project := los16BriefingStore(t)

	// Created at turn 0, deliberately the highest base weight: it can only lose
	// if turn-based decay actually runs.
	if _, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "los16 banana curve calibration",
		Project: project, Confidence: 1.0, Importance: 5,
		CreatedAt: time.Now().Add(-90 * 24 * time.Hour), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert old learning: %v", err)
	}

	for i := 0; i < 190; i++ {
		if _, err := s.IncrementTurnCount(project); err != nil {
			t.Fatalf("increment turn count: %v", err)
		}
	}
	if _, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "los16 quantum harbor inspection",
		Project: project, Confidence: 1.0, Importance: 3,
		CreatedAt: time.Now(), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert fresh learning: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := s.IncrementTurnCount(project); err != nil {
			t.Fatalf("increment turn count: %v", err)
		}
	}

	gen := New(s, 3)
	gen.SetMaxPerCategory(1)
	text := gen.Generate(project)

	if !strings.Contains(text, "los16 quantum harbor") {
		t.Errorf("fresh learning (turn 190 of 200) missing from the briefing; the highest-scored gotcha was rendered instead")
	}
	if strings.Contains(text, "los16 banana curve") {
		t.Errorf("learning created at turn 0 still rendered after 200 project turns: turn-based decay is not applied")
	}
}

// The end-to-end test above depends on where the profile happens to start in
// the rendered text. This one pins the boundary logic itself, at every offset
// around a 2-byte and a 3-byte rune.
func TestLos16TruncateBytesKeepsRuneBoundaries(t *testing.T) {
	// bytes: a a e2 82 ac c3 a4 b b b
	text := "aa" + "€" + "ä" + "bbb"

	tests := []struct {
		max  int
		want string
	}{
		{len(text), text},
		{9, "aa€äbb"},
		{6, "aa€"},
		{5, "aa€"},
		{4, "aa"},
		{3, "aa"},
		{2, "aa"},
		{1, "a"},
		{0, ""},
	}
	for _, tt := range tests {
		got := truncateBytes(text, tt.max)
		if got != tt.want {
			t.Errorf("truncateBytes(%q, %d) = %q, want %q", text, tt.max, got, tt.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncateBytes(%q, %d) is not valid UTF-8", text, tt.max)
		}
	}
}

// YM-34 side effect: once turn decay is live, the fade cut must not retire old
// low-weight learnings just because their decayed score reached the floor
// (TurnBasedDecay floors at exactly fadeThreshold).
func TestLos16BriefingDoesNotFadeOnTurnDecay(t *testing.T) {
	s, project := los16BriefingStore(t)

	if _, err := s.InsertLearning(&models.Learning{
		Category: "relationship", Content: "los16 quiet workshop habit",
		Project: project, Confidence: 1.0, Importance: 1,
		CreatedAt: time.Now().Add(-200 * 24 * time.Hour), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, err := s.IncrementTurnCount(project); err != nil {
			t.Fatalf("increment turn count: %v", err)
		}
	}

	text := New(s, 3).Generate(project)
	if !strings.Contains(text, "los16 quiet workshop habit") {
		t.Errorf("a 500-turn-old relationship learning disappeared from the briefing: the fade cut compares the decayed score")
	}
}

// YM-108: the hard 20KB limit sliced raw bytes, splitting a multi-byte rune.
func TestLos16BriefingTruncationKeepsValidUTF8(t *testing.T) {
	s, project := los16BriefingStore(t)

	// The user profile is the only briefing section without a per-item cap, so
	// it is what pushes the text past the byte limit.
	if err := s.SavePersonaDirective(&models.PersonaDirective{
		UserID:      "user_profile",
		Directive:   "x" + strings.Repeat("ä", 30000),
		TraitsHash:  "los16",
		GeneratedAt: time.Now(),
		ModelUsed:   "opus",
	}); err != nil {
		t.Fatalf("save persona directive: %v", err)
	}

	text := New(s, 3).Generate(project)
	if !strings.HasSuffix(text, "(briefing truncated)") {
		t.Fatalf("briefing was not truncated (%d bytes), fixture too small", len(text))
	}
	if !utf8.ValidString(text) {
		t.Errorf("truncated briefing is not valid UTF-8: the byte limit split a multi-byte rune")
	}
}

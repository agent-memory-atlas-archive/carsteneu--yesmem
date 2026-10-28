package daemon

import (
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-288 (Los 16b): loadTopPivotMoments sorts by the turn-decayed score, because
// GetActiveLearnings enriches CurrentTurnCount. A 200-day-old moment loses to a
// recent one even when its intrinsic weight is far higher. Whether past moments
// should age at all is an open product decision (reported, not decided here), so
// this test pins today's semantics: if the ordering ever changes, it changes on
// purpose.
func TestLos16bPivotMomentsAge(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	project := "/los16b/pivot-project"

	if _, err := s.InsertLearning(&models.Learning{
		Category: "pivot_moment", Content: "los16b old landmark",
		Project: project, Confidence: 1.0, Importance: 5, EmotionalIntensity: 1.0,
		CreatedAt: time.Now().Add(-200 * 24 * time.Hour), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert old pivot: %v", err)
	}
	for i := 0; i < 90; i++ {
		if _, err := s.IncrementTurnCount(project); err != nil {
			t.Fatalf("increment turn count: %v", err)
		}
	}
	if _, err := s.InsertLearning(&models.Learning{
		Category: "pivot_moment", Content: "los16b recent landmark",
		Project: project, Confidence: 1.0, Importance: 1,
		CreatedAt: time.Now(), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert recent pivot: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := s.IncrementTurnCount(project); err != nil {
			t.Fatalf("increment turn count: %v", err)
		}
	}

	pivots := loadTopPivotMoments(s, 2)
	if len(pivots) != 2 {
		t.Fatalf("expected 2 pivot moments, got %d", len(pivots))
	}
	if pivots[0].Content != "los16b recent landmark" {
		t.Errorf("first pivot = %q, want the recent one: pivot moments are ordered by the turn-decayed score",
			pivots[0].Content)
	}
}

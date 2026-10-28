package storage

import (
	"fmt"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// los16cSessionIDs is far beyond SQLite's variable limit (32766 on a default
// build), so a single IN list cannot carry them.
const los16cSessionIDs = 40000

// TestGetActiveLearningsBySessionIDsChunksLos16c (YM-343 [BRF-B13], #105220):
// the read built one placeholder per session ID, so a caller with more IDs than
// SQLite's variable limit got "too many SQL variables" and no rows at all.
func TestGetActiveLearningsBySessionIDsChunksLos16c(t *testing.T) {
	s := mustOpen(t)

	const wanted = "los16c-wanted-session"
	id, err := s.InsertLearning(&models.Learning{
		SessionID:  wanted,
		Category:   "gotcha",
		Content:    "los16c chunking marker",
		Confidence: 1.0,
		ModelUsed:  "test",
		Source:     "llm_extracted",
	})
	if err != nil {
		t.Fatalf("insert marker learning: %v", err)
	}

	ids := make([]string, 0, los16cSessionIDs+1)
	for i := 0; i < los16cSessionIDs; i++ {
		if i == los16cSessionIDs/2 {
			ids = append(ids, wanted)
		}
		ids = append(ids, fmt.Sprintf("los16c-empty-%05d", i))
	}

	got, err := s.GetActiveLearningsBySessionIDs(ids)
	if err != nil {
		t.Fatalf("GetActiveLearningsBySessionIDs with %d ids: %v", len(ids), err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("expected exactly the marker learning %d, got %d rows", id, len(got))
	}
}

// TestGetActiveLearningsBySessionIDsDedupesLos16c guards the batching's edge:
// a single IN list deduplicated repeated IDs implicitly, so batching must not
// return the same row once per batch.
func TestGetActiveLearningsBySessionIDsDedupesLos16c(t *testing.T) {
	s := mustOpen(t)

	const sessionID = "los16c-duplicate-session"
	id, err := s.InsertLearning(&models.Learning{
		SessionID:  sessionID,
		Category:   "gotcha",
		Content:    "los16c dedupe marker",
		Confidence: 1.0,
		ModelUsed:  "test",
		Source:     "llm_extracted",
	})
	if err != nil {
		t.Fatalf("insert marker learning: %v", err)
	}

	// Far enough apart that the two occurrences land in different batches.
	ids := make([]string, 0, 1001)
	ids = append(ids, sessionID)
	for i := 0; i < 999; i++ {
		ids = append(ids, fmt.Sprintf("los16c-empty-dedupe-%03d", i))
	}
	ids = append(ids, sessionID)

	got, err := s.GetActiveLearningsBySessionIDs(ids)
	if err != nil {
		t.Fatalf("GetActiveLearningsBySessionIDs: %v", err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("repeated session ID must yield one row, got %d rows", len(got))
	}
}

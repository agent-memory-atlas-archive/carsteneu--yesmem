package daemon

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// los10cMaxGetLearningsLimit is the cap the handler must enforce on a caller
// supplied limit. Kept as a literal so the RED run compiles against the
// unpatched handler (which has no such constant).
const los10cMaxGetLearningsLimit = 200

// los10cLearningsFromResponse counts the learnings in a get_learnings result.
func los10cLearningsFromResponse(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var rows []models.Learning
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse get_learnings result: %v (%s)", err, raw)
	}
	return len(rows)
}

// TestGetLearningsClampsCallerLimitLos10c (YM-305 [MEM-F37]): the handler fetched
// limit*5 rows. With limit = 2^61 the multiply wraps negative, GetActiveLearnings
// then omits its LIMIT clause and the handler returns the whole table.
func TestGetLearningsClampsCallerLimitLos10c(t *testing.T) {
	h, s := mustHandler(t)
	const rows = 250
	for i := 0; i < rows; i++ {
		// unfinished skips the dedup stage, so every row stays distinguishable
		insertAgedLearning(t, s, &models.Learning{
			Content:  fmt.Sprintf("los10c limit row %d", i),
			Category: "unfinished",
			Source:   "llm_extracted",
		})
	}

	resp := h.Handle(Request{
		Method: "get_learnings",
		Params: map[string]any{
			"category": "unfinished",
			"limit":    float64(int64(1) << 61),
		},
	})
	if resp.Error != "" {
		t.Fatalf("get_learnings with a huge limit: %s", resp.Error)
	}
	if got := los10cLearningsFromResponse(t, resp.Result); got > los10cMaxGetLearningsLimit {
		t.Errorf("a huge limit returned %d learnings, want at most %d", got, los10cMaxGetLearningsLimit)
	}
}

// TestGetLearningsRejectsNegativeLimitLos10c: a negative limit reached
// learnings[:limit] and panicked the handler goroutine.
func TestGetLearningsRejectsNegativeLimitLos10c(t *testing.T) {
	h, s := mustHandler(t)
	insertAgedLearning(t, s, &models.Learning{
		Content:  "los10c negative limit row",
		Category: "gotcha",
		Source:   "llm_extracted",
	})

	resp := h.Handle(Request{
		Method: "get_learnings",
		Params: map[string]any{
			"category": "gotcha",
			"limit":    float64(-5),
		},
	})
	if resp.Error != "" {
		t.Fatalf("get_learnings with a negative limit: %s", resp.Error)
	}
}

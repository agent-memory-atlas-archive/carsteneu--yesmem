package setup

import (
	"os"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// TestLos62c_StatusUsesSharedSupersedeStats pins finding 3 of YM-445. The status
// surface carried its own query (`superseded_by > 0`) while `yesmem stats`
// printed LearningStats.SupersededCount, so the two commands reported the same
// quantity with different numbers and different words. The definition lives in
// storage now; status must read it from there instead of counting the column
// itself.
func TestLos62c_StatusUsesSharedSupersedeStats(t *testing.T) {
	src, err := os.ReadFile("status.go")
	if err != nil {
		t.Fatalf("read status.go: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, "GetLearningStatsF") {
		t.Error("status.go does not read the supersede counts from GetLearningStatsF: the surface keeps its own definition")
	}
	for _, banned := range []string{
		"superseded_by > 0",
		"COUNT(*) FROM learnings WHERE superseded_by",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("status.go still counts learnings itself (%q): that is the second definition YM-445 removed", banned)
		}
	}
}

// TestLos62c_SupersedeSummaryLineMatchesStats pins the rendering to the shared
// struct: every number in the line is a field of LearningStats, so the line and
// `yesmem stats` cannot disagree while the struct is the only source.
func TestLos62c_SupersedeSummaryLineMatchesStats(t *testing.T) {
	st := &storage.LearningStats{
		SupersededCount:         6,
		ReplacedCount:           1,
		RetiredCount:            5,
		DeletedPredecessorCount: 2,
		TotalCount:              42,
	}
	line := supersedeSummaryLine(st)
	want := "Superseded: 6 (1 replaced, 5 retired), deleted predecessor: 2, Total: 42"
	if line != want {
		t.Errorf("supersedeSummaryLine = %q, want %q", line, want)
	}

	// Nothing retired: the line is dropped instead of printing zeros.
	if got := supersedeSummaryLine(&storage.LearningStats{}); got != "" {
		t.Errorf("supersedeSummaryLine of an all-zero count = %q, want an empty line", got)
	}
	if got := supersedeSummaryLine(nil); got != "" {
		t.Errorf("supersedeSummaryLine(nil) = %q, want an empty line", got)
	}
}

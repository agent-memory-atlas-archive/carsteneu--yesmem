package briefing

import "github.com/carsteneu/yesmem/internal/models"

// fadeThreshold is the minimum score for a learning to appear in the briefing.
// Learnings below this are "faded" — still in DB and searchable, but not shown.
const fadeThreshold = 0.1

// filterFaded removes learnings that carry too little value to show.
// Unfinished and pivot_moment items are never faded.
//
// The comparison uses the intrinsic score, not the decayed one: turn-based
// decay exists to rank, and it floors at exactly this threshold, so a decayed
// score would retire every old low-weight learning from the briefing entirely
// (measured on the live DB: 1073 learnings, 217 of them in one project).
func filterFaded(learnings []models.Learning, threshold float64) []models.Learning {
	if threshold <= 0 {
		return learnings
	}
	filtered := make([]models.Learning, 0, len(learnings))
	for _, l := range learnings {
		if l.Category == "unfinished" || l.Category == "pivot_moment" || models.ComputeIntrinsicScore(&l) >= threshold {
			filtered = append(filtered, l)
		}
	}
	return filtered
}

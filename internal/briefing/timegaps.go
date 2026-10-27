package briefing

import (
	"fmt"
	"sort"
	"time"
)

// computeTimeGaps finds gaps > 7 days between sessions and returns
// translated strings like "14 days in February".
func computeTimeGaps(s Strings, startedAts []time.Time) []string {
	if len(startedAts) < 2 {
		return nil
	}

	sorted := make([]time.Time, len(startedAts))
	copy(sorted, startedAts)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Before(sorted[j])
	})

	var gaps []string
	for i := 1; i < len(sorted); i++ {
		diff := sorted[i].Sub(sorted[i-1])
		days := int(diff.Hours() / 24)
		if days > 7 {
			gaps = append(gaps, formatTimeGap(s, days, sorted[i-1].Month()))
		}
	}
	return gaps
}

// formatTimeGap renders one gap note, e.g. "14 days in January".
func formatTimeGap(s Strings, days int, month time.Month) string {
	return fmt.Sprintf(s.LabelDaysInMonthFmt, days, monthName(s, month))
}

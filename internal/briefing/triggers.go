package briefing

import (
	"fmt"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// TriggerMatch is a learning whose trigger_rule fired.
type TriggerMatch struct {
	Learning models.Learning
	Reason   string // prefix-free urgency label, e.g. "morgen", "in 2 Tagen"
}

// checkDeadlineTriggers finds learnings with approaching deadlines (within 3 days).
// It is a pure lookup: the 12h cooldown (last_hit_at) is written by the delivery
// path only (Generator.MarkDeadlineHits), because background generations never
// reach a session and must not silence a deadline.
// Returns at most 3 matches.
func checkDeadlineTriggers(store *storage.Store, project string, s Strings) []TriggerMatch {
	now := time.Now()
	// Lower bound keeps long-expired deadline rows out of the row limit: the
	// query orders by trigger_rule and the caller's window starts yesterday.
	minRule := "deadline:" + now.AddDate(0, 0, -1).Format("2006-01-02")
	learnings, err := store.GetTriggeredLearnings(project, "deadline:", minRule)
	if err != nil || len(learnings) == 0 {
		return nil
	}

	var matches []TriggerMatch

	for _, l := range learnings {
		dateStr := strings.TrimPrefix(l.TriggerRule, "deadline:")
		deadline, err := time.ParseInLocation("2006-01-02", dateStr, now.Location())
		if err != nil {
			continue
		}

		// Calendar-day distance: a local midnight-to-midnight step is not 24h on a
		// DST day, so the value is counted on date parts, not on a duration.
		daysUntil := deadlineDaysUntil(now, deadline)
		if daysUntil > 3 || daysUntil < -1 {
			continue // only trigger within 3 days before and 1 day after deadline
		}

		reason := formatDeadlineReason(s, daysUntil)
		matches = append(matches, TriggerMatch{Learning: l, Reason: reason})

		if len(matches) >= 3 {
			break
		}
	}

	return matches
}

// deadlineDaysUntil returns the distance between the calendar day of now and the
// calendar day of deadline, in whole days. The date parts are rebuilt in UTC: a
// local midnight-to-midnight step lasts 23h or 25h across a DST change, so
// dividing that wall-clock duration by 24 moves the result off the day. In
// spring the value falls short (0.9583 for "tomorrow", rendered as "today"), in
// autumn it exceeds it (3.0417 for "in 3 days", which then failed the 3-day
// window and dropped the deadline entirely).
func deadlineDaysUntil(now, deadline time.Time) float64 {
	ny, nm, nd := now.Date()
	dy, dm, dd := deadline.Date()
	from := time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC)
	to := time.Date(dy, dm, dd, 0, 0, 0, 0, time.UTC)
	return to.Sub(from).Hours() / 24
}

// formatDeadlineReason returns a translated deadline proximity note.
// The returned text carries no "Deadline" prefix: the open-work template already
// renders "(<label>: <reason>)".
func formatDeadlineReason(s Strings, daysUntil float64) string {
	switch {
	case daysUntil < 0:
		return s.DeadlineOverdue
	case daysUntil < 1:
		return s.DeadlineToday
	case daysUntil < 2:
		return s.DeadlineTomorrow
	default:
		return fmt.Sprintf(s.DeadlineInDaysFmt, int(daysUntil))
	}
}

// containsLearning checks if a learning ID is already in the slice.
func containsLearning(learnings []models.Learning, id int64) bool {
	for _, l := range learnings {
		if l.ID == id {
			return true
		}
	}
	return false
}

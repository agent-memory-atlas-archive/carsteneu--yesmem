package briefing

import (
	"testing"
	"time"
)

// YM-249: formatDeadline built its date with the literal layout "%s %02d.%02d.",
// which is the German day.month order regardless of the installation language
// (pin: no hardcoded language in a user-facing string). The translated weekday
// and month tables already exist, so the layout can name both.
func TestFormatDeadlineFollowsTheStringLanguage(t *testing.T) {
	english := formatDeadline(DefaultStrings(), "deadline:2026-03-28")
	if english != "Sat 28 March" {
		t.Errorf("formatDeadline (English) = %q, want %q", english, "Sat 28 March")
	}

	german := DefaultStrings()
	german.WeekdaysShort = []string{"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"}
	german.MonthNames = []string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"}
	german.DeadlineDateFormat = "%s %d. %s"
	if got := formatDeadline(german, "deadline:2026-03-28"); got != "Sa 28. März" {
		t.Errorf("formatDeadline (German) = %q, want %q", got, "Sa 28. März")
	}

	// A strings.yaml written before the layout existed leaves the field empty.
	// The date must still render, in the English default.
	emptyLayout := DefaultStrings()
	emptyLayout.DeadlineDateFormat = ""
	if got := formatDeadline(emptyLayout, "deadline:2026-03-28"); got != "Sat 28 March" {
		t.Errorf("formatDeadline (no layout) = %q, want the default layout", got)
	}

	// A malformed weekday/month table falls back to English instead of dropping
	// the date: the row must still read as a deadline.
	broken := DefaultStrings()
	broken.WeekdaysShort = []string{"nur ein Tag"}
	broken.MonthNames = nil
	if got := formatDeadline(broken, "deadline:2026-03-28"); got != "Sat 28 March" {
		t.Errorf("formatDeadline (broken tables) = %q, want the English fallback", got)
	}

	// Non-deadline triggers keep rendering nothing.
	if got := formatDeadline(DefaultStrings(), "Freitext trigger"); got != "" {
		t.Errorf("formatDeadline(non-deadline) = %q, want empty", got)
	}
}

// YM-249: the day distance was derived by dividing a wall-clock duration by 24.
// On a DST day that duration is 23h or 25h for a one-day step, so the note
// shifted by one day: a deadline "tomorrow" read as "today" in spring (23h) and
// "tomorrow" read as "in 1 days" in autumn (25h). The distance is a calendar
// question, so it must be counted on date parts.
func TestDeadlineDaysUntilCountsCalendarDaysAcrossDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tz database on this host: " + err.Error())
	}
	at := func(y int, m time.Month, d, hour int) time.Time {
		return time.Date(y, m, d, hour, 0, 0, 0, berlin)
	}
	midnight := func(y int, m time.Month, d int) time.Time {
		return at(y, m, d, 0)
	}

	cases := []struct {
		name     string
		now      time.Time
		deadline time.Time
		want     float64
	}{
		// Spring forward (29.03.2026): 23 hours between the two midnights.
		{"spring, deadline tomorrow", at(2026, time.March, 29, 12), midnight(2026, time.March, 30), 1},
		{"spring, deadline today", at(2026, time.March, 29, 12), midnight(2026, time.March, 29), 0},
		{"spring, deadline in 3 days", at(2026, time.March, 29, 12), midnight(2026, time.April, 1), 3},
		// Fall back (25.10.2026): 25 hours between the two midnights.
		{"autumn, deadline tomorrow", at(2026, time.October, 25, 12), midnight(2026, time.October, 26), 1},
		// The autumn half of the old defect was not a wrong label but a dropped
		// deadline: 3.0417 days exceeded the 3-day trigger window.
		{"autumn, deadline in 3 days", at(2026, time.October, 25, 12), midnight(2026, time.October, 28), 3},
		{"autumn, deadline today", at(2026, time.October, 25, 12), midnight(2026, time.October, 25), 0},
		{"autumn, deadline yesterday", at(2026, time.October, 25, 12), midnight(2026, time.October, 24), -1},
		// A plain day has to keep working.
		{"ordinary day", at(2026, time.June, 10, 9), midnight(2026, time.June, 12), 2},
		// The rendered note is what the user sees.
	}
	for _, c := range cases {
		if got := deadlineDaysUntil(c.now, c.deadline); got != c.want {
			t.Errorf("%s: deadlineDaysUntil = %v, want %v", c.name, got, c.want)
		}
	}

	// The label that reaches the briefing list is the visible half of the bug.
	if got := formatDeadlineReason(DefaultStrings(), deadlineDaysUntil(at(2026, time.March, 29, 12), midnight(2026, time.March, 30))); got != "tomorrow" {
		t.Errorf("note for a deadline tomorrow across the spring transition = %q, want tomorrow", got)
	}
	// Autumn: the same three-day case must land inside the trigger window, which
	// the fractional value failed.
	if days := deadlineDaysUntil(at(2026, time.October, 25, 12), midnight(2026, time.October, 28)); days > 3 {
		t.Errorf("a deadline in 3 days across the autumn transition sits at %v days and falls out of the trigger window", days)
	}
}

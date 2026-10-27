package briefing

import (
	"fmt"
	"time"
)

// Built-in English defaults for the relative-time labels. DefaultStrings uses
// them, and relativeTime falls back to them when an installation's strings.yaml
// predates a key.
const (
	defaultRelativeMinutesFmt = "%d min ago"
	defaultRelativeHoursFmt   = "%dh ago"
	defaultRelativeYesterday  = "yesterday"
	defaultRelativeDaysFmt    = "%d days ago"
	defaultRelativeWeeksFmt   = "%d wks ago"
	defaultRelativeMonthsFmt  = "%d mo ago"
)

// relativeTime formats a timestamp as a relative time in the briefing language.
// The wording lives in strings.yaml, not in the code: a hardcoded language
// leaked German into every briefing regardless of the configured language
// (audit 16.10.2026, YM-328, Pin 4). An empty format falls back to the built-in
// English default, the same guard weekdayShort uses for a malformed
// weekdays_short list.
func relativeTime(s Strings, t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf(pick(s.RelativeMinutesFmt, defaultRelativeMinutesFmt), int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf(pick(s.RelativeHoursFmt, defaultRelativeHoursFmt), int(d.Hours()))
	case d < 48*time.Hour:
		return pick(s.RelativeYesterday, defaultRelativeYesterday)
	case d < 7*24*time.Hour:
		return fmt.Sprintf(pick(s.RelativeDaysFmt, defaultRelativeDaysFmt), int(d.Hours()/24))
	case d < 30*24*time.Hour:
		return fmt.Sprintf(pick(s.RelativeWeeksFmt, defaultRelativeWeeksFmt), int(d.Hours()/24/7))
	default:
		return fmt.Sprintf(pick(s.RelativeMonthsFmt, defaultRelativeMonthsFmt), int(d.Hours()/24/30))
	}
}

// pick returns the translated value, or the fallback when a strings.yaml written
// before the key existed carries no value for it.
func pick(translated, fallback string) string {
	if translated != "" {
		return translated
	}
	return fallback
}

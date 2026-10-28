package briefing

import (
	"testing"
	"time"
)

// relativeTime rendered the relative ages hard in German ("vor 4 Tagen"),
// whatever language the briefing was written in (audit 16.10.2026, YM-328,
// Pin 4 Internationalisierung). The wording belongs in strings.yaml, like the
// recency and deadline labels next to it.
func TestRelativeTime_FollowsStrings(t *testing.T) {
	now := time.Now()
	en := DefaultStrings()
	cases := []struct {
		d    time.Duration
		want string
	}{
		{15 * time.Minute, "15 min ago"},
		{3 * time.Hour, "3h ago"},
		{30 * time.Hour, "yesterday"},
		{4 * 24 * time.Hour, "4 days ago"},
		{14 * 24 * time.Hour, "2 wks ago"},
		{60 * 24 * time.Hour, "2 mo ago"},
	}
	for _, c := range cases {
		if got := relativeTime(en, now.Add(-c.d)); got != c.want {
			t.Errorf("english, -%v: got %q, want %q", c.d, got, c.want)
		}
	}

	// German stays reachable through strings.yaml, not through the code.
	de := Strings{
		RelativeMinutesFmt: "vor %d Min",
		RelativeHoursFmt:   "vor %dh",
		RelativeYesterday:  "gestern",
		RelativeDaysFmt:    "vor %d Tagen",
		RelativeWeeksFmt:   "vor %d Wo",
		RelativeMonthsFmt:  "vor %d Mo",
	}
	if got := relativeTime(de, now.Add(-4*24*time.Hour)); got != "vor 4 Tagen" {
		t.Errorf("german: got %q, want %q", got, "vor 4 Tagen")
	}
}

// An older installation has a strings.yaml without the new keys. An empty value
// must fall back to the built-in English default instead of rendering an empty
// or verb-less phrase.
func TestRelativeTime_EmptyFormatFallsBackToDefault(t *testing.T) {
	if got := relativeTime(Strings{}, time.Now().Add(-4*24*time.Hour)); got != "4 days ago" {
		t.Errorf("empty strings: got %q, want the English default", got)
	}
	if got := relativeTime(Strings{}, time.Time{}); got != "?" {
		t.Errorf("zero time: got %q, want ?", got)
	}
}

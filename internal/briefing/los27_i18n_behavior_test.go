package briefing

import (
	"strings"
	"testing"
)

// YM-111: briefing text was wired to German at several formatting sites while
// the rest of the briefing uses English defaults plus the translatable
// briefing.Strings layer. These assertions pin the default output to the
// translatable layer's English defaults.

func TestLos27_FormatAbsenceDefaultIsEnglish(t *testing.T) {
	tests := []struct {
		hours float64
		want  string
	}{
		{5, "5 hours away"},
		{23, "23 hours away"},
		{25, "away since yesterday"},
		{47, "away since yesterday"},
		{49, "2 days away"},
		{120, "5 days away"},
	}
	for _, tt := range tests {
		got := formatAbsence(DefaultStrings(), tt.hours)
		if got != tt.want {
			t.Errorf("formatAbsence(%v) = %q, want %q", tt.hours, got, tt.want)
		}
		if strings.Contains(got, "Du warst") {
			t.Errorf("formatAbsence(%v) still renders the hardcoded German note: %q", tt.hours, got)
		}
	}
}

func TestLos27_FormatDeadlineWeekdaysAreEnglish(t *testing.T) {
	tests := []struct {
		trigger string
		want    string
	}{
		{"deadline:2026-03-28", "Sat 28 March"},
		{"deadline:2026-03-24", "Tue 24 March"},
		{"deadline:2026-01-01", "Thu 1 January"},
		{"deadline:invalid", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := formatDeadline(DefaultStrings(), tt.trigger)
		if got != tt.want {
			t.Errorf("formatDeadline(%q) = %q, want %q", tt.trigger, got, tt.want)
		}
	}
}

func TestLos27_FormatDeadlineReasonIsEnglish(t *testing.T) {
	tests := []struct {
		daysUntil float64
		want      string
	}{
		{-0.5, "overdue"},
		{0.3, "today"},
		{0.9, "today"},
		{1.2, "tomorrow"},
		{1.9, "tomorrow"},
		{2.5, "in 2 days"},
	}
	for _, tt := range tests {
		got := formatDeadlineReason(DefaultStrings(), tt.daysUntil)
		if got != tt.want {
			t.Errorf("formatDeadlineReason(%v) = %q, want %q", tt.daysUntil, got, tt.want)
		}
	}
}

func TestLos27_FormatPinnedBlockIsEnglish(t *testing.T) {
	got := FormatPinnedBlock(nil, nil)
	if got != "" {
		t.Fatalf("no pins should render nothing, got %q", got)
	}
}

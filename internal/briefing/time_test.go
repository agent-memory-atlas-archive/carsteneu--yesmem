package briefing

import (
	"testing"
	"time"
)

// The relative-time cases run against DefaultStrings, the base language of
// strings.yaml; TestRelativeTime_FollowsStrings covers the translated path.
func TestRelativeTime_Zero(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Time{})
	if got != "?" {
		t.Errorf("expected '?' for zero time, got %q", got)
	}
}

func TestRelativeTime_Minutes(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Now().Add(-15*time.Minute))
	if got != "15 min ago" {
		t.Errorf("expected '15 min ago', got %q", got)
	}
}

func TestRelativeTime_ZeroMinutes(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Now().Add(-30*time.Second))
	if got != "0 min ago" {
		t.Errorf("expected '0 min ago', got %q", got)
	}
}

func TestRelativeTime_Hours(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Now().Add(-3*time.Hour))
	if got != "3h ago" {
		t.Errorf("expected '3h ago', got %q", got)
	}
}

func TestRelativeTime_Yesterday(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Now().Add(-30*time.Hour))
	if got != "yesterday" {
		t.Errorf("expected 'yesterday', got %q", got)
	}
}

func TestRelativeTime_Days(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Now().Add(-4*24*time.Hour))
	if got != "4 days ago" {
		t.Errorf("expected '4 days ago', got %q", got)
	}
}

func TestRelativeTime_Weeks(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Now().Add(-14*24*time.Hour))
	if got != "2 wks ago" {
		t.Errorf("expected '2 wks ago', got %q", got)
	}
}

func TestRelativeTime_Months(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Now().Add(-60*24*time.Hour))
	if got != "2 mo ago" {
		t.Errorf("expected '2 mo ago', got %q", got)
	}
}

func TestRelativeTime_Boundary_HourMinus1(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Now().Add(-59*time.Minute))
	if got != "59 min ago" {
		t.Errorf("expected '59 min ago' at 59m boundary, got %q", got)
	}
}

func TestRelativeTime_Boundary_ExactHour(t *testing.T) {
	got := relativeTime(DefaultStrings(), time.Now().Add(-60*time.Minute))
	if got != "1h ago" {
		t.Errorf("expected '1h ago' at exact hour boundary, got %q", got)
	}
}

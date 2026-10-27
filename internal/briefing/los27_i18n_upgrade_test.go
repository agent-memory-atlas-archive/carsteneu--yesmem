package briefing

import (
	"os"
	"path/filepath"
	"testing"
)

// YM-111: strings.yaml files written before the new keys existed must keep
// working. LoadStrings starts from DefaultStrings, so a legacy file without
// absence/deadline/pin keys must fall back to the English defaults instead of
// rendering empty labels.
func TestLos27_LegacyStringsFileKeepsNewDefaults(t *testing.T) {
	legacy := `greeting: ""
tool_hint: "Use these tools when you need relevant context."
known_pitfalls: "Pitfalls I've stepped into:"
open_work: "What I haven't finished yet:"
`
	path := filepath.Join(t.TempDir(), "strings.yaml")
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy strings: %v", err)
	}

	s, err := LoadStrings(path)
	if err != nil {
		t.Fatalf("load legacy strings: %v", err)
	}

	def := DefaultStrings()
	if s.PinBlockIntro != def.PinBlockIntro {
		t.Errorf("pin_block_intro = %q, want the default %q", s.PinBlockIntro, def.PinBlockIntro)
	}
	if s.AbsenceHoursFmt != def.AbsenceHoursFmt {
		t.Errorf("absence_hours_fmt = %q, want the default %q", s.AbsenceHoursFmt, def.AbsenceHoursFmt)
	}
	if len(s.WeekdaysShort) != 7 {
		t.Errorf("weekdays_short has %d entries, want 7", len(s.WeekdaysShort))
	}
	if s.CapIdeasHeader != def.CapIdeasHeader {
		t.Errorf("cap_ideas_header = %q, want the default %q", s.CapIdeasHeader, def.CapIdeasHeader)
	}
	if s.OpenWork != "What I haven't finished yet:" {
		t.Errorf("existing key was not read from the file: %q", s.OpenWork)
	}
}

// A weekdays_short list of the wrong length must not panic the briefing.
func TestLos27_WeekdayShortGuardsBrokenList(t *testing.T) {
	s := DefaultStrings()
	s.WeekdaysShort = []string{"Mo"}

	if got := weekdayShort(s, 6); got != "Sat" {
		t.Errorf("weekdayShort with a broken list = %q, want the English fallback %q", got, "Sat")
	}
}

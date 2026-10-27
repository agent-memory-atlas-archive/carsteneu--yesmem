package briefing

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-111: the hardcoded briefing texts had no key in the translatable
// briefing.Strings layer, so strings.yaml could not override them. These tests
// pin the missing surface: every rendered text must come from Strings.

// withStringsFile points the package-level strings lookup at a temporary
// strings.yaml and restores the previous path afterwards.
func withStringsFile(t *testing.T, s Strings) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "strings.yaml")
	if err := SaveStrings(path, s); err != nil {
		t.Fatalf("save strings: %v", err)
	}
	prev := DefaultStringsPath()
	SetStringsPath(path)
	t.Cleanup(func() { SetStringsPath(prev) })
}

func TestLos27_PinnedBlockFollowsStringsFile(t *testing.T) {
	s := DefaultStrings()
	s.PinBlockIntro = "Diese Anweisungen gelten bis sie explizit aufgehoben werden:"
	s.PinBlockRemove = "Zum Entfernen: unpin(id, scope)"
	withStringsFile(t, s)

	got := FormatPinnedBlock([]storage.PinnedLearning{{ID: 1, Content: "always TDD"}}, nil)

	if !strings.Contains(got, "Diese Anweisungen gelten") {
		t.Errorf("pin block ignored the translated intro: %q", got)
	}
	if !strings.Contains(got, "Zum Entfernen: unpin(id, scope)") {
		t.Errorf("pin block ignored the translated remove line: %q", got)
	}
	if !strings.Contains(got, "[pin:1] always TDD") {
		t.Errorf("pin content missing: %q", got)
	}
}

func TestLos27_PinnedBlockDefaultsAreEnglish(t *testing.T) {
	prev := DefaultStringsPath()
	SetStringsPath("")
	t.Cleanup(func() { SetStringsPath(prev) })

	got := FormatPinnedBlock([]storage.PinnedLearning{{ID: 1, Content: "always TDD"}}, nil)

	if strings.Contains(got, "Diese Anweisungen") || strings.Contains(got, "Zum Entfernen") {
		t.Errorf("default pin block is still hardcoded German: %q", got)
	}
}

func TestLos27_MetamemoryTextFollowsStrings(t *testing.T) {
	s := DefaultStrings()
	s.MetamemoryHeader = "Wissensqualitaet:"
	s.MetamemoryProvenFmt = "%d/%d belegt"
	s.MetamemoryFragileFmt = "%d fragil"
	s.MetamemoryOutdatedFmt = "%d veraltet"

	got := metamemoryText(s, 10, 4, 2, 1)

	for _, want := range []string{"Wissensqualitaet:", "4/10 belegt", "2 fragil", "1 veraltet"} {
		if !strings.Contains(got, want) {
			t.Errorf("metamemory text missing %q: %q", want, got)
		}
	}
}

func TestLos27_OpenWorkTemplateDeadlineLabelIsTranslatable(t *testing.T) {
	s := DefaultStrings()
	s.LabelDeadline = "Frist"

	got := renderTemplate("openwork", tmplOpenWork, s, OpenWorkData{
		Items: []OpenWorkItem{{Content: "ship it", Deadline: "today"}},
	})

	if !strings.Contains(got, "(Frist: today)") {
		t.Errorf("deadline label is not taken from Strings: %q", got)
	}
}

func TestLos27_MilestonesTemplateHasNoHardcodedGerman(t *testing.T) {
	got := renderTemplate("milestones", tmplMilestones, DefaultStrings(), MilestoneData{
		Items: []MilestoneItem{{Ago: "3d", IntensityLabel: "vivid", Flavor: "shipped the fix"}},
	})

	if strings.Contains(got, "Mehr über") {
		t.Errorf("milestones template still carries hardcoded German: %q", got)
	}
	if !strings.Contains(got, `more via get_learnings("pivot_moment")`) {
		t.Errorf("milestones template lost its overflow hint: %q", got)
	}
}

func TestLos27_CapIdeasTextFollowsStrings(t *testing.T) {
	s := DefaultStrings()
	s.CapIdeasHeader = "Cap-Vorschlaege:"
	s.CapIdeasConfirmLabel = "Bestaetigen"
	s.CapIdeasDismissLabel = "Verwerfen"
	s.CapIdeasDismissNote = "die [#ID] neben dem Vorschlag"

	got := capIdeasText(s, []models.Learning{{ID: 7, Content: "deploy flow", MatchCount: 4}})

	for _, want := range []string{"Cap-Vorschlaege:", "deploy flow", "seen 4x", "Bestaetigen: `remember(text=\"<intent>\", category=\"unfinished\", task_type=\"task\", supersedes=<ID>)`", "Verwerfen: `resolve(learning_id=<ID>)` — die [#ID] neben dem Vorschlag"} {
		if !strings.Contains(got, want) {
			t.Errorf("cap ideas block missing %q: %q", want, got)
		}
	}
}

// The tool-call syntax must survive any translation: a strings.yaml that
// translated the argument values would point the agent at filters that do not
// exist.
func TestLos27_CapIdeasToolCallsAreNotTranslatable(t *testing.T) {
	s := DefaultStrings()
	s.CapIdeasConfirmLabel = "Bestaetigen"
	s.CapIdeasDismissLabel = "Verwerfen"
	s.CapIdeasHeader = "Cap-Vorschlaege:"
	s.CapIdeasDismissNote = "Hinweis"

	got := capIdeasText(s, []models.Learning{{ID: 7, Content: "x", MatchCount: 2}})

	if !strings.Contains(got, `category="unfinished", task_type="task"`) {
		t.Errorf("cap-idea confirm call lost its argument syntax: %q", got)
	}
	if !strings.Contains(got, "resolve(learning_id=<ID>)") {
		t.Errorf("cap-idea dismiss call lost its argument syntax: %q", got)
	}
	if !strings.HasPrefix(got, "\nCap-Vorschlaege:\n\n") {
		t.Errorf("cap-idea block layout is not owned by the code: %q", got)
	}
}

func TestLos27_OpenWorkIdeasAndStaleLabelsAreTranslatable(t *testing.T) {
	s := DefaultStrings()
	s.OpenWorkIdeasLabel = "Ideen"
	s.OpenWorkStaleLabel = "veraltet"

	tests := []struct {
		name       string
		ideas      int
		stale      int
		wantInText []string
	}{
		{"only ideas", 3, 0, []string{"(+ 3 Ideen — get_learnings(category=\"unfinished\", task_type=\"idea\")"}},
		{"only stale", 0, 2, []string{"(+ 2 veraltet — get_learnings(category=\"unfinished\", task_type=\"stale\")"}},
		{"both", 3, 2, []string{"(+ 3 Ideen — get_learnings(category=\"unfinished\", task_type=\"idea\"); 2 veraltet — get_learnings(category=\"unfinished\", task_type=\"stale\")"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderTemplate("openwork", tmplOpenWork, s, OpenWorkData{
				Items:      []OpenWorkItem{{Content: "ship it"}},
				IdeaCount:  tt.ideas,
				StaleCount: tt.stale,
			})
			for _, want := range tt.wantInText {
				if !strings.Contains(got, want) {
					t.Errorf("open-work segment missing %q:\n%s", want, got)
				}
			}
			if tt.ideas == 0 && strings.Contains(got, "task_type=\"idea\"") {
				t.Errorf("idea segment rendered without ideas:\n%s", got)
			}
			if tt.stale == 0 && strings.Contains(got, "task_type=\"stale\"") {
				t.Errorf("stale segment rendered without stale entries:\n%s", got)
			}
		})
	}
}

func TestLos27_TimeGapLabelFollowsStrings(t *testing.T) {
	s := DefaultStrings()
	s.LabelDaysInMonthFmt = "%d Tage im %s"
	s.MonthNames = []string{"Januar", "Februar", "Maerz", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"}

	got := formatTimeGap(s, 14, 1)

	if got != "14 Tage im Januar" {
		t.Errorf("time gap note = %q, want %q", got, "14 Tage im Januar")
	}
	if def := formatTimeGap(DefaultStrings(), 14, 1); def != "14 days in January" {
		t.Errorf("default time gap note = %q, want %q", def, "14 days in January")
	}
}

// A truncated month_names list falls back to English instead of panicking.
func TestLos27_MonthNameGuardsBrokenList(t *testing.T) {
	s := DefaultStrings()
	s.MonthNames = []string{"Januar"}

	if got := monthName(s, 2); got != "February" {
		t.Errorf("monthName with a broken list = %q, want the English fallback %q", got, "February")
	}
}

func TestLos27_OpenWorkBringUpLineFollowsStrings(t *testing.T) {
	s := DefaultStrings()
	s.OpenWorkBringUp = "Sprich die Punkte aktiv an."

	got := renderTemplate("openwork", tmplOpenWork, s, OpenWorkData{
		Items: []OpenWorkItem{{Content: "ship it"}},
	})

	if !strings.Contains(got, s.AgentName+": Sprich die Punkte aktiv an.") {
		t.Errorf("open-work bring-up line is not taken from Strings: %q", got)
	}
}

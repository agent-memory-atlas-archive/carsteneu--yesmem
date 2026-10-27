package briefing

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-112 / Entscheidung D8: a present user profile used to suppress the persona
// directive entirely. Both must appear: the profile first, then a short
// directive (at most 5 rules while the profile already carries the detail).
func TestUserProfileAndPersonaDirectiveBothAppear(t *testing.T) {
	store, _ := storage.Open(":memory:")
	defer store.Close()

	store.UpsertSession(&models.Session{
		ID: "s1", Project: "/var/www/proj", ProjectShort: "proj",
		StartedAt: time.Now().Add(-2 * time.Hour), MessageCount: 20,
		JSONLPath: "/s1.jsonl", IndexedAt: time.Now(),
	})
	store.InsertLearning(&models.Learning{
		Category: "narrative", Project: "proj", SessionID: "s1",
		Content:       "Wir haben den Scanner gefixt.",
		SessionFlavor: "Debug-Session mit Scanner-Fix",
		CreatedAt:     time.Now().Add(-1 * time.Hour), ModelUsed: "haiku",
	})

	// Persona directive with more rules than the profile-path limit allows.
	store.SavePersonaDirective(&models.PersonaDirective{
		UserID: "default",
		Directive: "- Regel 1\n- Regel 2\n- Regel 3\n- Regel 4\n- Regel 5\n" +
			"- Regel 6\n- Regel 7\n- Regel 8",
		TraitsHash:  "abc123",
		GeneratedAt: time.Now(),
		ModelUsed:   "opus",
	})
	store.SavePersonaDirective(&models.PersonaDirective{
		UserID:      "user_profile",
		Directive:   "Ein erfahrener Go-Entwickler der TDD praktiziert.",
		TraitsHash:  "profilehash",
		GeneratedAt: time.Now(),
		ModelUsed:   "opus",
	})

	text := New(store, 3).Generate("/var/www/proj")

	if !strings.Contains(text, "erfahrener Go-Entwickler") {
		t.Fatalf("user profile not found in briefing:\n%s", text)
	}
	if !strings.Contains(text, "Arbeitsbeziehung:") {
		t.Fatalf("persona directive must not be suppressed by the user profile:\n%s", text)
	}
	profileIdx := strings.Index(text, "erfahrener Go-Entwickler")
	directiveIdx := strings.Index(text, "Arbeitsbeziehung:")
	if profileIdx > directiveIdx {
		t.Errorf("profile (pos %d) must appear before the persona directive (pos %d)", profileIdx, directiveIdx)
	}

	// Short directive while the profile carries the detail.
	for _, keep := range []string{"- Regel 1", "- Regel 5"} {
		if !strings.Contains(text, keep) {
			t.Errorf("directive rule %q missing:\n%s", keep, text)
		}
	}
	for _, drop := range []string{"- Regel 6", "- Regel 8"} {
		if strings.Contains(text, drop) {
			t.Errorf("directive rule %q should be cut while a profile is present:\n%s", drop, text)
		}
	}
	if !strings.Contains(text, `(3 more rules via get_learnings("preference"))`) {
		t.Errorf("missing overflow hint for the cut rules:\n%s", text)
	}
}

// Without a user profile the directive keeps its full limit of 10 rules.
func TestPersonaDirectiveWithoutProfileKeepsTenRules(t *testing.T) {
	store, _ := storage.Open(":memory:")
	defer store.Close()

	store.UpsertSession(&models.Session{
		ID: "s1", Project: "/var/www/proj", ProjectShort: "proj",
		StartedAt: time.Now().Add(-2 * time.Hour), MessageCount: 20,
		JSONLPath: "/s1.jsonl", IndexedAt: time.Now(),
	})
	rules := make([]string, 0, 12)
	for i := 1; i <= 12; i++ {
		rules = append(rules, "- Regel "+string(rune('0'+i/10))+string(rune('0'+i%10)))
	}
	store.SavePersonaDirective(&models.PersonaDirective{
		UserID:      "default",
		Directive:   strings.Join(rules, "\n"),
		TraitsHash:  "abc123",
		GeneratedAt: time.Now(),
		ModelUsed:   "opus",
	})

	text := New(store, 3).Generate("/var/www/proj")

	if !strings.Contains(text, "- Regel 10") {
		t.Errorf("without a profile the directive should keep 10 rules:\n%s", text)
	}
	if strings.Contains(text, "- Regel 12") {
		t.Errorf("eleventh/twelfth rule must be cut:\n%s", text)
	}
}

func TestUserProfileEmptyWhenNotSet(t *testing.T) {
	store, _ := storage.Open(":memory:")
	defer store.Close()

	store.UpsertSession(&models.Session{
		ID: "s1", Project: "/var/www/proj", ProjectShort: "proj",
		StartedAt: time.Now().Add(-2 * time.Hour), MessageCount: 20,
		JSONLPath: "/s1.jsonl", IndexedAt: time.Now(),
	})

	gen := New(store, 3)
	profile := gen.loadUserProfile()

	if profile != "" {
		t.Errorf("expected empty profile when not set, got %q", profile)
	}
}

func TestLoadUserProfile(t *testing.T) {
	store, _ := storage.Open(":memory:")
	defer store.Close()

	store.SavePersonaDirective(&models.PersonaDirective{
		UserID:      "user_profile",
		Directive:   "Ein pragmatischer Architekt.",
		TraitsHash:  "hash123",
		GeneratedAt: time.Now(),
		ModelUsed:   "opus",
	})

	gen := New(store, 3)
	profile := gen.loadUserProfile()

	if profile != "Ein pragmatischer Architekt." {
		t.Errorf("expected profile text, got %q", profile)
	}
}

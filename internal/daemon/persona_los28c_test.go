package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-280 (COG-C24): the user-profile prompt interpolated trait_key and
// trait_value with %s. The storage layer rejects control characters in trait
// text, but a value carrying quotes or a backslash still reached the prompt
// unescaped, unlike the two sibling synthesis prompts
// (briefing/persona.go:67, extraction/persona.go:189), which quote with %q.
func TestSynthesizeUserProfileQuotesTraitValues(t *testing.T) {
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	s.InsertLearning(&models.Learning{
		Category: "preference", Content: "User bevorzugt kurze Antworten",
		Source: "user_stated", CreatedAt: time.Now(), ModelUsed: "opus",
	})
	s.UpsertPersonaTrait(&models.PersonaTrait{
		UserID: "default", Dimension: "expertise", TraitKey: "go",
		TraitValue: `hoch ("senior"), C:\pfad`,
		Confidence: 0.8, Source: "learning_scan", EvidenceCount: 3,
	})

	mock := &capturingMockClient{completeResp: "Ein erfahrener Entwickler."}
	synthesizeUserProfile(s, mock)

	if mock.lastUserMessage == "" {
		t.Fatal("prompt was not built")
	}
	if !strings.Contains(mock.lastUserMessage, `- "go" = "hoch (\"senior\"), C:\\pfad"`) {
		t.Errorf("trait key and value are not quoted and escaped in the prompt:\n%s", mock.lastUserMessage)
	}
	if strings.Contains(mock.lastUserMessage, "- go: hoch") {
		t.Errorf("trait line is still interpolated unquoted:\n%s", mock.lastUserMessage)
	}
}

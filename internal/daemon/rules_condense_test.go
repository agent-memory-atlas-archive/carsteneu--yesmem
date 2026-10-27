package daemon

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-25: a doc-sync run re-ingests every registered source, and ingest.Run
// upserts the source without content. UpsertDocSource wrote full_content
// unconditionally, so the condensed rules block of the "rules" source was
// blanked on the next sync. (Storage-level guard: TestUpsertDocSource_... in
// internal/storage.)

func writeClaudeMd(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	return path
}

// YM-25: an empty LLM answer must not be stored as the rules block. Storing it
// also stored a fresh hash, which then blocked every later re-condensation.
func TestCondenseRules_RejectsEmptyLLMOutput(t *testing.T) {
	_, s := mustHandler(t)

	claude := writeClaudeMd(t, "# Rules\n\nNever commit secrets.\n")
	client := &mockLLM{response: "   \n\t\n"}

	if _, _, err := CondenseRules(claude, "p", client, s); err == nil {
		t.Fatalf("empty condensation was accepted, want an error")
	}

	if got := s.GetRulesHash("p"); got != "" {
		t.Errorf("empty condensation recorded hash %q — later retries are blocked", got)
	}
}

// YM-25: the live damage is a rules row with a matching hash and an empty
// full_content. The hash guard turned that state into a permanent dead end —
// the content has to be regenerated.
func TestCondenseRules_RegeneratesWhenStoredContentIsEmpty(t *testing.T) {
	_, s := mustHandler(t)

	claude := writeClaudeMd(t, "# Rules\n\nNever commit secrets.\n")
	expanded, err := expandWithLinks(claude, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(expanded)))

	id, err := s.UpsertDocSource(&storage.DocSource{Name: "rules", Project: "p", Path: claude})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	// Broken live state: hash recorded, content gone.
	if err := s.SaveRulesContent(id, "", hash); err != nil {
		t.Fatalf("save rules content: %v", err)
	}

	client := &mockLLM{response: "Never commit secrets. Always run the tests."}
	condensed, _, err := CondenseRules(claude, "p", client, s)
	if err != nil {
		t.Fatalf("CondenseRules with empty stored content: %v — the rules block stays dead", err)
	}
	if condensed == "" {
		t.Fatalf("empty condensation returned")
	}
	if got := s.GetRulesContent("p"); got != "Never commit secrets. Always run the tests." {
		t.Errorf("rules content = %q, want the regenerated block", got)
	}
}

// Guard for the hash shortcut itself: unchanged source AND intact content must
// still short-circuit (no needless LLM call).
func TestCondenseRules_UnchangedSourceIsSkipped(t *testing.T) {
	_, s := mustHandler(t)

	claude := writeClaudeMd(t, "# Rules\n\nNever commit secrets.\n")
	expanded, err := expandWithLinks(claude, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(expanded)))

	id, err := s.UpsertDocSource(&storage.DocSource{Name: "rules", Project: "p", Path: claude})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	if err := s.SaveRulesContent(id, "Never commit secrets.", hash); err != nil {
		t.Fatalf("save rules content: %v", err)
	}

	client := &mockLLM{response: "should not be used"}
	_, _, err = CondenseRules(claude, "p", client, s)
	if err == nil || err.Error() != "unchanged" {
		t.Fatalf("err = %v, want 'unchanged'", err)
	}
	if got := s.GetRulesContent("p"); got != "Never commit secrets." {
		t.Errorf("rules content changed to %q", got)
	}
}

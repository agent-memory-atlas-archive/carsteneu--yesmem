package briefing

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// YM-288 (Los 16b): the tools paragraph lives inside the awakening section, and
// Generate renders that section first. Cutting the raw briefing at the tools
// line therefore threw away every later section (person, knowledge, project,
// open work), so the refiner only ever saw the arrival paragraph.
func TestLos16bStripKeepsSectionsAfterToolsBlock(t *testing.T) {
	s, project := los16BriefingStore(t)

	if _, err := s.InsertLearning(&models.Learning{
		Category: "gotcha", Content: "los16b cobalt anchor registry",
		Project: project, Confidence: 1.0, Importance: 5,
		CreatedAt: time.Now(), Source: "llm_extracted",
	}); err != nil {
		t.Fatalf("insert learning: %v", err)
	}

	raw := New(s, 3).Generate(project)
	learningAt := strings.Index(raw, "los16b cobalt anchor registry")
	toolsAt := strings.Index(raw, "expand_context(query)")
	if learningAt < 0 {
		t.Fatalf("fixture: briefing does not contain the learning:\n%s", raw)
	}
	if toolsAt < 0 {
		t.Fatalf("fixture: briefing does not contain the tools catalog:\n%s", raw)
	}
	if learningAt < toolsAt {
		t.Fatalf("fixture: learning renders before the tools paragraph, the test proves nothing:\n%s", raw)
	}

	got := stripToolsBlock(raw)
	if !strings.Contains(got, "los16b cobalt anchor registry") {
		t.Errorf("learning lost: the strip removed every section after the tools paragraph")
	}
	if strings.Contains(got, "expand_context(query)") {
		t.Errorf("tools catalog survived the strip:\n%s", got)
	}
}

// The paragraph boundary towards the documentation line (and the next section)
// is one line, not a blank line: tmplAwakening renders label, catalog, hint and
// then the doc-sources line back to back.
func TestLos16bStripStopsAtTheDocumentationLine(t *testing.T) {
	catalog := "search(query), hybrid_search(query), deep_search(query), remember(text)."
	raw := "I'm back. 2 times now.\n\n---\nPulse: kept.\n\n" +
		"How I can work with my memory:\n" + catalog + "\nUse these tools.\n" +
		"3 Documentation sources available - docs_search(query) to look up.\n" +
		"## Project\n- los16b project body\n"

	got := stripToolsBlock(raw)
	for _, gone := range []string{catalog, "How I can work with my memory:", "Use these tools."} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived the strip:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"Pulse: kept.", "3 Documentation sources available", "## Project", "los16b project body"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q was lost by the strip:\n%s", kept, got)
		}
	}
}

// A learning that quotes the catalog mid sentence is not the tools paragraph.
func TestLos16bStripIgnoresCatalogQuoteInProse(t *testing.T) {
	catalog := "search(query), hybrid_search(query), deep_search(query), remember(text)."
	raw := "Arrival.\n\nA learning quoting the tools: " + catalog + "\nAfter the quote.\n\n" +
		"How I can work with my memory:\n" + catalog + "\nUse these tools.\n\n## Project\n- body\n"

	got := stripToolsBlock(raw)
	if !strings.Contains(got, "A learning quoting the tools") {
		t.Errorf("prose quoting the catalog was eaten:\n%s", got)
	}
	if !strings.Contains(got, "After the quote.") {
		t.Errorf("line after the quoted catalog was eaten:\n%s", got)
	}
	if strings.Contains(got, "How I can work with my memory:") {
		t.Errorf("the real tools paragraph survived:\n%s", got)
	}
	if !strings.Contains(got, "## Project") {
		t.Errorf("section after the tools paragraph was lost:\n%s", got)
	}
}

// A tools paragraph whose label was translated (strings.yaml is written by an
// LLM pass, so the label text is arbitrary) must still be stripped. The catalog
// line beneath it is rendered verbatim by tmplAwakening and is never translated,
// so it is the stable anchor.
func TestLos16bStripFindsTranslatedToolsLabel(t *testing.T) {
	raw := "I'm back. 3 times now.\n\n---\nPulse: something.\n\n" +
		"Wie ich mit meinem Gedaechtnis arbeite:\n" +
		"search(query), hybrid_search(query), deep_search(query), query_facts(entity, action, keyword), docs_search(query), related_to_file(path), remember(text), get_session(id), get_learnings(category), expand_context(query), set_plan(plan), update_plan(), get_plan(), complete_plan().\n" +
		"Benutze diese Werkzeuge, wenn du Kontext brauchst.\n\n" +
		"Was ich noch offen habe:\n- los16b open task magenta\n"

	got := stripToolsBlock(raw)
	if strings.Contains(got, "expand_context(query)") {
		t.Errorf("tools catalog survived the strip:\n%s", got)
	}
	if strings.Contains(got, "Wie ich mit meinem Gedaechtnis arbeite:") {
		t.Errorf("translated tools label survived the strip:\n%s", got)
	}
	if !strings.Contains(got, "los16b open task magenta") {
		t.Errorf("open work after the tools paragraph was lost:\n%s", got)
	}
	if !strings.Contains(got, "Pulse: something.") {
		t.Errorf("content before the tools paragraph was lost:\n%s", got)
	}
}

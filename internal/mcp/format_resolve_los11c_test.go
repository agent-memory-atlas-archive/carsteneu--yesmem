package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// YM-251 (Los 11c): when nothing is resolved, the response now carries the
// candidates. The formatter has to surface them, otherwise the agent sees only
// "Not resolved" and cannot pick an id.
func TestFormatResolve_Los11c_RendersCandidates(t *testing.T) {
	raw := json.RawMessage(`{"resolved":false,"message":"Not resolved: 2 unfinished items contain all 3 search terms, pass the learning_id instead","candidates":[{"id":66092,"content":"Scheduled job proxy-health-check was triggered on 2026-05-11","project":"yesmem"},{"id":1,"content":"proxy rules cache rebuild","project":"yesmem"}]}`)

	got := formatResolve(raw)
	for _, want := range []string{"Not resolved:", "#66092", "#1", "proxy rules cache rebuild"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatted candidates missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatResolve_Los11c_NoCandidatesKeepsMessage(t *testing.T) {
	raw := json.RawMessage(`{"resolved":false,"message":"Not resolved: no matching unfinished items found","candidates":[]}`)

	if got := formatResolve(raw); got != "Not resolved: no matching unfinished items found" {
		t.Errorf("unexpected rendering: %q", got)
	}
}

func TestShortenForList_Los11c_KeepsRunesIntact(t *testing.T) {
	content := strings.Repeat("ä", 120)

	got := shortenForList(content, 50)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
	if runes := utf8.RuneCountInString(strings.TrimSuffix(got, "...")); runes != 50 {
		t.Errorf("expected 50 runes before the ellipsis, got %d", runes)
	}
}

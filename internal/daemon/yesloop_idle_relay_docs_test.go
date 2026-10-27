package daemon

import (
	"os"
	"strings"
	"testing"
)

// idleSkillDocs are the two bundled copies that quote the idle relay verbatim.
var idleSkillDocs = []string{
	"../../skills/bundled-skills/yesloop/SKILL.md",
	"../../skills/bundled-commands/opencode/yesloop.md",
}

// TestIdleRelayMarkerFormsMatchSkillDocs (YM-203): since YM-149 the idle layer
// accepts the header form, a dedicated marker line and the phase template's
// Status line. The relay used to ask for a bare "[x] in scratchpad" and both
// skill copies repeated that wording, so an agent following the docs could miss
// the accepted forms. The docs claim to quote the relay exactly, so the check is
// a verbatim comparison, and the named forms are exercised against the checker.
func TestIdleRelayMarkerFormsMatchSkillDocs(t *testing.T) {
	// The forms the relay names must be the forms the checker accepts.
	headerForm := "### Phase 1: ANALYZE [x]\n**Status:** IN PROGRESS\n"
	if !phaseMarkedDoneByX(headerForm, 1) {
		t.Error("checker does not accept the header form the relay names")
	}
	dedicatedForm := "### Phase 1: ANALYZE\n**Status:** IN PROGRESS\n- [x] Phase 1\n"
	if !phaseMarkedDoneByX(dedicatedForm, 1) {
		t.Error("checker does not accept the dedicated marker line the relay names")
	}
	statusForm := "### Phase 1: ANALYZE\n**Status:** COMPLETE\n"
	if countMarkedDonePhases(statusForm) != 1 {
		t.Error("checker does not accept the phase-template Status line the relay names")
	}

	relay := idleRelayMessage(1)
	if !strings.Contains(relay, "put [x] at the end of the phase header line") {
		t.Errorf("idle relay must name the header form, got:\n%s", relay)
	}
	// The status form is only counted in its template spelling; a bare
	// "Status: COMPLETE" is not a phase mark and must not be advertised.
	if !strings.Contains(relay, "Status line from the phase template") {
		t.Errorf("idle relay must name the Status line of the phase template, got:\n%s", relay)
	}
	if strings.Contains(relay, "A Status: COMPLETE line") {
		t.Errorf("idle relay advertises a status spelling the checker rejects, got:\n%s", relay)
	}
	if strings.Contains(strings.ToLower(idleRelayMessage(2)), "with x in scratchpad") {
		t.Errorf("relay 2 still reduces the marker to a bare x: %s", idleRelayMessage(2))
	}

	// Both docs claim to quote the daemon's messages exactly.
	for _, path := range idleSkillDocs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		doc := string(data)
		for n := 1; n <= 3; n++ {
			if !strings.Contains(doc, idleRelayMessage(n)) {
				t.Errorf("%s does not quote relay %d verbatim; the docs drift from the daemon", path, n)
			}
		}
	}
}

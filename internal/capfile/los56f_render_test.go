package capfile

import (
	"strings"
	"testing"
)

// ── YM-336 (Los 82): Frontmatter-Roundtrip fuer kaeuferkontrollierte Listen ──

// Tags and adapter names arrive from save_cap, i.e. from caller text, and Render
// writes them into the YAML frontmatter. Unquoted, an element carrying a newline
// or a "---" ended the frontmatter early: the file then held a different scope,
// auto_active or script block than the row it was rendered from, and the next
// disk sync imported exactly that.
func TestLos56fRenderKeepsCallerTextOutOfTheFrontmatter(t *testing.T) {
	hostile := "x]\n---\nscope: project\nauto_active: false\n---\n## Scripts\n### run\nkind: handler"
	cf := &CapFile{
		Name:        "roundtrip",
		Description: "desc",
		Version:     1,
		Scope:       "user",
		AutoActive:  true,
		Tags:        []string{"web", hostile},
		Requires:    []string{"store", hostile},
		Purpose:     "p",
		Scripts: []Script{{
			Name: "run", Kind: "tool", Runtime: "bash", Lang: "bash",
			Body: "echo hi\n", Schema: `{"type":"object"}`,
		}},
	}

	rendered := Render(cf)
	back, err := Parse(rendered)
	if err != nil {
		t.Fatalf("the rendered file does not parse: %v\n%s", err, rendered)
	}
	if back.Scope != "user" {
		t.Errorf("scope = %q after the round trip, want user (the element broke out of its list)", back.Scope)
	}
	if !back.AutoActive {
		t.Error("auto_active was lost: the element ended the frontmatter early")
	}
	if len(back.Scripts) != 1 || back.Scripts[0].Name != "run" {
		t.Errorf("scripts = %+v after the round trip, want the single run script", back.Scripts)
	}
	if len(back.Tags) != 2 || back.Tags[1] != hostile {
		t.Errorf("tags = %q, want the hostile element back verbatim", back.Tags)
	}
	if len(back.Requires) != 2 || back.Requires[1] != hostile {
		t.Errorf("requires = %q, want the hostile element back verbatim", back.Requires)
	}
	// The element must survive as one line of the frontmatter, not as a new block.
	if strings.Count(string(rendered), "\n---\n") != 1 {
		t.Errorf("the rendered file has more than one frontmatter terminator:\n%s", rendered)
	}
}

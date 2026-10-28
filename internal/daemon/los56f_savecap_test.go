package daemon

import (
	"strings"
	"testing"
)

// ── YM-319 (Los 77): Skriptnamen brauchen einen Zeichensatz ────────────────

// A script name is the fn an execute_cap caller passes, and it is printed in the
// capability catalog. save_cap checked only for emptiness, so a name with a
// pipe or a newline broke out of the catalog row and was then shown filtered
// (capTableCell replaces those characters) while execute_cap compares the raw
// name: the catalog named a function that no longer resolves.
func TestLos56fScriptNameCharsetIsEnforced(t *testing.T) {
	bad := []string{
		"my tool",     // space: not passable as fn in the documented form
		"a|b",         // ends the catalog table row
		"x\ny",        // ends the catalog row and the surrounding block
		"a<b",         // opens a tag inside <caps-available>
		"path/script", // path separator
		"UPPER",       // not the shared cap-name alphabet
		"führe",       // non-ASCII
		"1st",         // a leading digit
	}
	for _, name := range bad {
		scripts := []ScriptMeta{{Name: name, Kind: "tool", Runtime: "repl", Body: "async () => 1"}}
		if _, err := normalizeScripts(scripts); err == nil {
			t.Errorf("script name %q was accepted", name)
		}
	}

	// The names every bundled and every installed cap actually uses stay valid.
	for _, name := range []string{"reddit_fetch", "run", "x_bash", "a1", "proxy_health"} {
		scripts := []ScriptMeta{{Name: name, Kind: "tool", Runtime: "repl", Body: "async () => 1"}}
		if _, err := normalizeScripts(scripts); err != nil {
			t.Errorf("script name %q was refused: %v", name, err)
		}
	}

	// The refusal has to name the script, otherwise a caller with ten scripts
	// cannot tell which one save_cap rejected.
	_, err := normalizeScripts([]ScriptMeta{
		{Name: "good_tool", Kind: "tool", Runtime: "repl", Body: "async () => 1"},
		{Name: "bad|tool", Kind: "tool", Runtime: "repl", Body: "async () => 2"},
	})
	if err == nil {
		t.Fatal("a name with a pipe was accepted")
	}
	if !strings.Contains(err.Error(), "bad|tool") {
		t.Errorf("error %q does not name the rejected script", err)
	}
}

// ── YM-336 (Los 82): Supersede-Suche mit rohem project ─────────────────────

// save_cap looks for the cap it replaces with GetActiveLearnings("cap", project).
// An empty project is no filter at all there, so a global save took the first
// active cap of that name in ANY project and retired it: a user who saved
// "reddit_fetch" globally lost the cap of the project whose row happened to come
// first, silently, because the supersede path reports success.
func TestLos56fGlobalSaveDoesNotSupersedeAnotherProjectsCap(t *testing.T) {
	h, s := mustHandler(t)
	const scripts = `[{"name":"run","kind":"tool","runtime":"repl","body":"async () => 1"}]`

	if resp := h.handleSaveCap(map[string]any{
		"name": "shared_tool", "description": "project beta cap", "project": "beta", "scripts": scripts,
	}); resp.Error != "" {
		t.Fatalf("save beta cap: %s", resp.Error)
	}
	if resp := h.handleSaveCap(map[string]any{
		"name": "shared_tool", "description": "global cap", "scripts": scripts,
	}); resp.Error != "" {
		t.Fatalf("save global cap: %s", resp.Error)
	}

	active, err := s.GetActiveLearnings("cap", "", "", "", 0)
	if err != nil {
		t.Fatalf("GetActiveLearnings: %v", err)
	}
	projects := map[string]bool{}
	for _, l := range active {
		meta, err := ParseCapMeta(l.Context)
		if err != nil || meta.Name != "shared_tool" {
			continue
		}
		projects[l.Project] = true
	}
	if !projects["beta"] {
		t.Error("the global save superseded beta's cap of the same name")
	}
	if !projects[""] {
		t.Error("the global cap itself was not stored")
	}
}

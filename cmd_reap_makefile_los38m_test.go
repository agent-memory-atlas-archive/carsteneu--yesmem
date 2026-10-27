package main

import (
	"os"
	"strings"
	"testing"
)

// makefileRecipe returns the body of a Makefile target, or "" when the target does
// not exist. The target ends at the next recipe-less line that starts a new rule.
func makefileRecipe(t *testing.T, target string) string {
	t.Helper()
	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	var body []string
	inTarget := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, target+":") {
			inTarget = true
			body = append(body, line)
			continue
		}
		if !inTarget {
			continue
		}
		if line != "" && !strings.HasPrefix(line, "\t") {
			break
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}

// YM-312: `make reap-stale-mcp` swept the process table with pgrep -f and killed
// every match whose /proc/<pid>/exe read "(deleted)". Nothing verified that a hit
// was a `yesmem mcp` helper: a grep whose arguments carried "yesmem mcp", a search
// command, or a helper of another installation all qualified, and the pattern also
// missed helpers that the status scanner (argv-anchored) does report. The rule now
// delegates to the binary, where the same scan that `yesmem status` uses decides.
func TestMakefileReapDelegatesToYesmemBinary(t *testing.T) {
	recipe := makefileRecipe(t, "reap-stale-mcp")
	if recipe == "" {
		t.Fatal("Makefile has no reap-stale-mcp target")
	}
	// Quoted or bare: the path may contain spaces, so only the delegation counts.
	if !strings.Contains(strings.ReplaceAll(recipe, "\"", ""), "$(INSTALL) reap-stale-mcp") {
		t.Errorf("reap-stale-mcp does not delegate to the installed binary:\n%s", recipe)
	}
	for _, forbidden := range []string{"readlink /proc", "pgrep"} {
		if strings.Contains(recipe, forbidden) {
			t.Errorf("reap-stale-mcp still sweeps the process table itself (%q): the identity check lives in the binary, not in shell:\n%s", forbidden, recipe)
		}
	}
}

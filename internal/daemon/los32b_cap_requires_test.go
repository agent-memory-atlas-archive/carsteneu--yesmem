package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// los32bScriptsParam renders a single bash script as the JSON array save_cap takes.
func los32bScriptsParam(t *testing.T, runtime, body string) string {
	t.Helper()
	raw, err := json.Marshal([]ScriptMeta{{
		Name: "run", Kind: "tool", Runtime: runtime, Lang: runtime,
		Body: body, Schema: `{"type":"object"}`,
	}})
	if err != nil {
		t.Fatalf("render scripts: %v", err)
	}
	return string(raw)
}

func los32bRequiresHas(requires []string, want string) bool {
	for _, r := range requires {
		if r == want {
			return true
		}
	}
	return false
}

// YM-264: requires was recomputed from a bare `store(` substring. Bash caps call
// the adapter primitive as a shell command (`store '{"op":...}'`) — never with a
// parenthesis — so every bash cap that reads or writes cap_store stored
// requires=nil, and the recompute threw away the list the CAP.md had declared.
func TestHandleSaveCap_DetectsTheBashStoreCall(t *testing.T) {
	h, s := mustHandler(t)
	resp := h.handleSaveCap(map[string]any{
		"name":        "bash_storing",
		"description": "reads its input from cap_store",
		"scripts":     los32bScriptsParam(t, "bash", "store '{\"op\":\"query\",\"table\":\"posts\"}'\n"),
	})
	if resp.Error != "" {
		t.Fatalf("save_cap: %s", resp.Error)
	}

	meta := storedCapMeta(t, s, "bash_storing")
	if meta == nil {
		t.Fatal("cap was not stored")
	}
	if !los32bRequiresHas(meta.Requires, "store") {
		t.Errorf("Requires = %v, want it to contain \"store\": a bash cap calling `store ...` needs the adapter", meta.Requires)
	}
}

// A word that merely contains the primitive name is not a call: a comment, a
// variable name or an echo must not declare an adapter.
func TestHandleSaveCap_DoesNotMistakeAWordForABashStoreCall(t *testing.T) {
	h, s := mustHandler(t)
	resp := h.handleSaveCap(map[string]any{
		"name":        "bash_talking",
		"description": "only mentions the primitive",
		"scripts":     los32bScriptsParam(t, "bash", "echo \"restore the backup\"\nmy_store=1\n# store nothing\n"),
	})
	if resp.Error != "" {
		t.Fatalf("save_cap: %s", resp.Error)
	}

	meta := storedCapMeta(t, s, "bash_talking")
	if meta == nil {
		t.Fatal("cap was not stored")
	}
	if los32bRequiresHas(meta.Requires, "store") {
		t.Errorf("Requires = %v, want no \"store\": the body only mentions the word", meta.Requires)
	}
}

// A declared requires list is the author's contract (capfile.Parse already lets
// it win over detection). save_cap ignored the parameter and always recomputed.
func TestHandleSaveCap_KeepsTheDeclaredRequires(t *testing.T) {
	h, s := mustHandler(t)
	resp := h.handleSaveCap(map[string]any{
		"name":        "declared",
		"description": "declares its adapters",
		"requires":    "file,web",
		"scripts":     los32bScriptsParam(t, "repl", "async () => store(\"q\")"),
	})
	if resp.Error != "" {
		t.Fatalf("save_cap: %s", resp.Error)
	}

	meta := storedCapMeta(t, s, "declared")
	if meta == nil {
		t.Fatal("cap was not stored")
	}
	if got := strings.Join(meta.Requires, ","); got != "file,web" {
		t.Errorf("Requires = %q, want the declared \"file,web\" — save_cap must not replace a declared list", got)
	}
}

// Without a declared list the detection still runs, for the repl form.
func TestHandleSaveCap_DetectsReplAdaptersWithoutADeclaration(t *testing.T) {
	h, s := mustHandler(t)
	resp := h.handleSaveCap(map[string]any{
		"name":        "detected",
		"description": "no declaration",
		"scripts":     los32bScriptsParam(t, "repl", "async () => store(\"q\")"),
	})
	if resp.Error != "" {
		t.Fatalf("save_cap: %s", resp.Error)
	}

	meta := storedCapMeta(t, s, "detected")
	if meta == nil {
		t.Fatal("cap was not stored")
	}
	if !los32bRequiresHas(meta.Requires, "store") {
		t.Errorf("Requires = %v, want \"store\" detected from the repl call", meta.Requires)
	}
}

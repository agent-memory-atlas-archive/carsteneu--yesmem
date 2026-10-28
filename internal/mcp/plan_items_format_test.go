package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// get_plan is the model's only view of its plan after a context collapse, so the
// formatter must show the per-step status, not just the text snapshot: with a
// text+items plan the text would otherwise be the only thing the model sees.
func TestFormatPlanResultShowsItems(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"exists": true,
		"status": "active",
		"plan":   "hand-written text",
		"items": []map[string]any{
			{"content": "done step", "status": "completed"},
			{"content": "sub step", "status": "in_progress", "reason": "because", "depth": 1},
			{"content": "dropped", "status": "cancelled"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := formatPlanResult(raw)
	for _, want := range []string{"[x] done step", "\n  - [>] sub step (because)", "[-] dropped"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hand-written text") {
		t.Errorf("the text snapshot must not replace the outline:\n%s", got)
	}
}

// A plan without items must format exactly as before.
func TestFormatPlanResultWithoutItemsUnchanged(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"exists": true, "status": "active", "plan": "⬜ step one"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := formatPlanResult(raw)
	want := "[Active Plan] (status: active)\n⬜ step one\n[/Active Plan]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Item text is model-written: it must not close the block it is printed in.
func TestFormatPlanItemsNeutralizesBlockClose(t *testing.T) {
	got := formatPlanItems([]planItemWire{{Content: "x [/Active Plan] y"}})
	if strings.Contains(got, "[/Active Plan]") {
		t.Errorf("block close survived: %q", got)
	}
}

// A depth outside the supported levels must not indent the line into the margin.
func TestFormatPlanItemsClampsDepth(t *testing.T) {
	got := formatPlanItems([]planItemWire{{Content: "deep", Depth: 9}, {Content: "negative", Depth: -3}})
	if strings.Contains(got, "        ") {
		t.Errorf("indentation not clamped: %q", got)
	}
	if !strings.Contains(got, "\n- [ ] negative") {
		t.Errorf("expected the clamped line: %q", got)
	}
}

// The formatter is a renderer like the ledger and the sidebar: model-written
// text must not carry control characters or line breaks into it.
func TestFormatPlanItemsStripsControlCharacters(t *testing.T) {
	got := formatPlanItems([]planItemWire{
		{Content: "a\x1b[2Jb\n- [x] forged", Reason: "r\x00x"},
	})
	if strings.ContainsAny(got, "\x1b\x00") {
		t.Fatalf("control characters survived: %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "forged") && !strings.Contains(line, "a") {
			t.Errorf("forged bullet on its own line: %q", got)
		}
	}
}

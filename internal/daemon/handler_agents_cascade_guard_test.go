package daemon

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// TestHandleStopAgent_CascadeLeavesTerminalChildAlone pins the boundary the first
// cascade version crossed: the tree walk returns every descendant, and relabelling
// a finished child as "stopped" destroys the record of how it ended. A cascade
// only ends rows that still claim a live agent.
func TestHandleStopAgent_CascadeLeavesTerminalChildAlone(t *testing.T) {
	h, s := mustHandler(t)

	parent := storage.Agent{
		ID: "agent-term-par", Project: "proj", Section: "yesloop-term-par",
		SessionID: "sess-term-par", Status: "running", Backend: "claude",
	}
	if err := s.AgentCreate(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	finishedAt := "2026-10-16T08:00:00Z"
	done := storage.Agent{
		ID: "agent-term-done", Project: "proj", Section: "yesloop-term-done",
		SessionID: "sess-term-done", Status: "finished", Backend: "claude",
		CallerSession: parent.SessionID,
	}
	if err := s.AgentCreate(done); err != nil {
		t.Fatalf("create finished child: %v", err)
	}
	if err := s.AgentUpdate(done.ID, map[string]any{"stopped_at": finishedAt, "progress": "finished normally"}); err != nil {
		t.Fatalf("seed the finished child's record: %v", err)
	}
	before, err := s.AgentGet(done.ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}

	if resp := h.handleStopAgent(map[string]any{"to": parent.ID}); resp.Error != "" {
		t.Fatalf("stop parent: %s", resp.Error)
	}

	got, err := s.AgentGet(done.ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if got.Status != "finished" {
		t.Errorf("status = %q, want finished: the cascade must not relabel a terminal row", got.Status)
	}
	if got.Progress != before.Progress {
		t.Errorf("progress = %q, want the original %q", got.Progress, before.Progress)
	}
	if got.StoppedAt != before.StoppedAt {
		t.Errorf("stopped_at = %q, want the original %q", got.StoppedAt, before.StoppedAt)
	}
}

// TestHandleUpdateAgent_ExpectedPIDRejectsNonNumeric closes the fail-open its first
// version had: a caller that names an expected pid in any other shape fell through
// to the unconditional write, which is the very path YM-202 removes.
func TestHandleUpdateAgent_ExpectedPIDRejectsNonNumeric(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-exit-4", Project: "proj", Section: "sec-exit-4",
		SessionID: "uuid-exit-4", PID: 555, Status: "running", Backend: "claude",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	for _, expected := range []any{"555", float64(0), float64(-1), true} {
		resp := h.handleUpdateAgent(map[string]any{
			"id":           "agent-exit-4",
			"expected_pid": expected,
			"fields":       map[string]any{"status": "error", "error": "exit: 1"},
		})
		if resp.Error == "" {
			t.Errorf("expected_pid %v (%T) was accepted on the unconditional path", expected, expected)
		}
	}

	got, err := s.AgentGet("agent-exit-4")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status != "running" || got.PID != 555 {
		t.Errorf("row = (status %q, pid %d), want it untouched", got.Status, got.PID)
	}
}

// TestDotCommandValue pins the escaping the binding relies on: only these two
// characters need a backslash in the sqlite3 CLI's double-quoted token, and a
// value the argument cannot carry is refused instead of bound to something else.
func TestDotCommandValue(t *testing.T) {
	ok := map[string]string{
		"ses_plain":     "ses_plain",
		"p'fresh":       "p'fresh",
		`a"b`:           `a\"b`,
		`c:\path`:       `c:\\path`,
		"with space":    "with space",
		"tab\tin value": "tab\tin value",
	}
	for in, want := range ok {
		got, err := dotCommandValue(in)
		if err != nil {
			t.Errorf("dotCommandValue(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("dotCommandValue(%q) = %q, want %q", in, got, want)
		}
	}

	for _, bad := range []string{"line\nbreak", "carriage\rreturn", "nul\x00byte"} {
		if _, err := dotCommandValue(bad); err == nil {
			t.Errorf("dotCommandValue(%q) accepted a value a dot-command argument cannot carry", bad)
		}
	}

	// A leading bind sigil would be read as a reference and bound as NULL.
	for _, sigil := range []string{"@other", ":other", "$other", "?1"} {
		if _, err := dotCommandValue(sigil); err == nil {
			t.Errorf("dotCommandValue(%q) accepted a bind sigil: the CLI would bind NULL", sigil)
		}
	}
}

// TestParameterArgs_RefusesUncarryableValues: the refusal reaches the query path
// instead of being printed to stdout by the CLI, where the row reader would take
// the usage text as data.
func TestParameterArgs_RefusesUncarryableValues(t *testing.T) {
	if _, err := parameterArgs([]sessionQueryParam{{name: "@parent", value: "a\nb"}}); err == nil {
		t.Fatalf("parameterArgs accepted a line break")
	}
	args, err := parameterArgs([]sessionQueryParam{{name: "@parent", value: "p'x"}})
	if err != nil {
		t.Fatalf("parameterArgs: %v", err)
	}
	if len(args) != 1 || !strings.Contains(args[0], `@parent`) || !strings.Contains(args[0], `"p'x"`) {
		t.Fatalf("args = %q, want one double-quoted binding", args)
	}
}

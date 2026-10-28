package proxy

import (
	"strconv"
	"testing"
)

// YM-147: every request re-recorded the sh() calls of the whole history, one
// synchronous daemon RPC per command. A long session therefore fired tens of
// thousands of record_repl_pattern calls per request and inflated every shape
// count by the number of requests that followed it. Only the newest assistant
// turn may be recorded.
func TestDetectReplPatternSuggestion_RecordsOnlyNewestTurn(t *testing.T) {
	var messages []any
	messages = append(messages, userMsg("start"))
	for i := 0; i < 50; i++ {
		messages = append(messages,
			makeAssistantRepl("await sh('sqlite3 history"+strconv.Itoa(i)+" q')"),
			userMsg("next"),
		)
	}
	messages = append(messages, makeAssistantRepl(
		"await sh('sqlite3 newest one'); await sh('sqlite3 newest two')",
	))

	m := &mockDaemon{}
	detectReplPatternSuggestion(messages, "proj", m.query)

	var examples []string
	for _, c := range m.calls {
		if c.method != "record_repl_pattern" {
			continue
		}
		example, _ := c.params["example"].(string)
		examples = append(examples, example)
	}
	if len(examples) != 2 {
		t.Fatalf("expected 2 record calls for the newest turn, got %d: %v", len(examples), examples)
	}
	for _, e := range examples {
		if e != "sqlite3 newest one" && e != "sqlite3 newest two" {
			t.Errorf("a historical command was re-recorded: %q", e)
		}
	}
}

// An assistant turn without REPL calls must not resurrect the commands of an
// older turn.
func TestDetectReplPatternSuggestion_DoesNotLookPastATextTurn(t *testing.T) {
	messages := []any{
		userMsg("start"),
		makeAssistantRepl("await sh('sqlite3 old q')"),
		userMsg("next"),
		assistantMsg("done, nothing to run"),
		userMsg("thanks"),
	}

	m := &mockDaemon{}
	detectReplPatternSuggestion(messages, "proj", m.query)
	if len(m.calls) != 0 {
		t.Fatalf("expected no record calls, got %+v", m.calls)
	}
}

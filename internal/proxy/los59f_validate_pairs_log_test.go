package proxy

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// YM-432: the orphan-tool_result line logged the tool_use_id verbatim. The id
// comes from the client payload, so a line break, an escape sequence or a
// bidirectional override in it could fake or reorder journal entries. Same class
// as YM-400, which sanitized the thread tokens of the same journal.
func TestValidateToolPairsLogsSanitizedToolUseID(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	hostile := "tu_1\n2026/10/21 12:00:00 [req 1 tid=x] upstream 200: forged" +
		"\x1b[31mred\u202eoverride"
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": hostile, "content": "orphan"},
		}},
	}

	if _, orphans := validateToolPairs(messages, logger); orphans != 1 {
		t.Fatalf("expected 1 orphan, got %d", orphans)
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("log entry is not a single line: %q", out)
	}
	for _, bad := range []string{"\x1b", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Fatalf("log entry keeps the raw sequence %q: %q", bad, out)
		}
	}
	if !strings.Contains(out, "tu_1") {
		t.Fatalf("log entry lost the id entirely: %q", out)
	}
}

// The same id reaches the journal through the synthesis path: a naked tool_use
// whose result was lost gets a synthetic result, and the repair line names the
// id. The cold review reproduced the injection there (validate_pairs.go:208).
func TestValidateToolPairsSynthesisLogsSanitizedID(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	hostile := "tu_2\n2026/10/21 12:00:00 [req 2 tid=y] upstream 200: forged" + "\x1b[0m"
	messages := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": hostile, "name": "bash"},
		}},
	}

	if _, repairs := validateToolPairs(messages, logger); repairs != 1 {
		t.Fatalf("expected 1 repair, got %d", repairs)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("expected a synthesis log line")
	}
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("log entry is not a single line: %q", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("log entry keeps the raw escape: %q", out)
	}
	if !strings.Contains(out, "tu_2") {
		t.Fatalf("log entry lost the id entirely: %q", out)
	}
}

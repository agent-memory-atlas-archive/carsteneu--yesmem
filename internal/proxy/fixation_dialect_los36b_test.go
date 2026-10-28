package proxy

import (
	"io"
	"log"
	"testing"
)

// opencode names its tools lowercase and its file argument "path"
// (tool/plugin/edit.ts, read.ts), Claude Code sends PascalCase and "file_path".
// The fixation analysis has to read the class, not the dialect.
func TestAnalyzeFixation_OpencodeFileRetries(t *testing.T) {
	var parts []any
	for i := 0; i < 12; i++ {
		parts = append(parts, los36bOpencodeToolUse("edit", map[string]any{"path": "/src/main.go"}))
		parts = append(parts, toolResult(false, "ok"))
	}
	msgs := buildMessages(parts...)

	analysis := AnalyzeFixation(msgs)
	// Twelve edits of one file count twice each (edit + result), capped at the
	// total message count.
	if analysis.FixationMessages != 24 || analysis.TotalMessages != 24 {
		t.Fatalf("expected 24 of 24 messages in fixation, got %+v", analysis)
	}
	if analysis.Ratio != 1 {
		t.Errorf("expected ratio 1 for a full fixation run, got %f", analysis.Ratio)
	}
}

// The edit/build/error cycle is the other error-based signal; it needs the
// opencode spellings on both the edit and the shell side.
func TestAnalyzeFixation_OpencodeEditBuildErrorCycles(t *testing.T) {
	var parts []any
	for i := 0; i < 6; i++ {
		parts = append(parts, los36bOpencodeToolUse("edit", map[string]any{"path": "/src/main.go"}))
		parts = append(parts, toolResult(false, "edited"))
		parts = append(parts, los36bOpencodeToolUse("shell", map[string]any{"command": "go build ./..."}))
		parts = append(parts, toolResult(true, "undefined: x"))
	}
	msgs := buildMessages(parts...)

	if got := countEditBuildErrorCycles(msgs, 6); got.runs == 0 {
		t.Fatalf("six opencode edit/build/error cycles must be counted, got %+v", got)
	}
}

// The file argument key differs per client and per tool (path, file_path,
// filePath); the retry counter has to read all of them or it counts nothing.
func TestCountFileRetries_OpencodePathKey(t *testing.T) {
	var parts []any
	for i := 0; i < 10; i++ {
		parts = append(parts, los36bOpencodeToolUse("write", map[string]any{"path": "/src/other.go"}))
		parts = append(parts, toolResult(false, "ok"))
	}
	msgs := buildMessages(parts...)

	retries := countFileRetries(msgs, 10)
	if retries["/src/other.go"] != 10 {
		t.Errorf("expected 10 retries for /src/other.go, got %v", retries)
	}
}

// A session id shorter than the log window used to slice past the end when the
// daemon call failed.
func TestPersistFixationRatio_ShortThreadIDDoesNotPanic(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0)}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a short thread id must not panic: %v", r)
		}
	}()
	s.persistFixationRatio("abc", 0.5)
}

// los36bOpencodeToolUse builds an opencode-dialect tool_use message.
func los36bOpencodeToolUse(name string, input map[string]any) map[string]any {
	return map[string]any{
		"role": "assistant",
		"content": []any{map[string]any{
			"type":  "tool_use",
			"id":    "call_" + name,
			"name":  name,
			"input": input,
		}},
	}
}

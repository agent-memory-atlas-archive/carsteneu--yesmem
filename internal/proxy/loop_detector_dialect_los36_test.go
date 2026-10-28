package proxy

import "testing"

// los36ToolUse builds an assistant tool_use block with a caller-supplied input,
// so the tests can express the opencode dialect (lowercase names, "path") next
// to the Claude Code dialect (PascalCase, "file_path").
func los36ToolUse(name string, input map[string]any) any {
	return map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "tool_" + name,
				"name":  name,
				"input": input,
			},
		},
	}
}

// YM-131: opencode sends lowercase tool names and "path" instead of
// "file_path". The extractor must read both dialects, otherwise edit/test/
// error cycles and build commands are invisible for every opencode session.
func TestExtractRecentToolCalls_OpencodeDialect(t *testing.T) {
	msgs := buildMessages(
		userMsg("fix it"),
		los36ToolUse("edit", map[string]any{"path": "/src/main.go", "oldString": "a", "newString": "b"}),
		toolResult(false, "edited"),
		los36ToolUse("shell", map[string]any{"command": "go test ./internal/proxy"}),
		toolResult(false, "ok"),
	)

	calls := extractRecentToolCalls(msgs, 12)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].FilePath != "/src/main.go" {
		t.Errorf("opencode edit must yield its path, got FilePath=%q", calls[0].FilePath)
	}
	if calls[1].Command != "go test ./internal/proxy" {
		t.Errorf("opencode shell must yield its command, got Command=%q", calls[1].Command)
	}
}

// The tool name stays the raw name it was sent under, so a signal can name the
// tool the way the client called it.
func TestExtractRecentToolCalls_OpencodeKeepsRawToolName(t *testing.T) {
	msgs := buildMessages(
		userMsg("go"),
		los36ToolUse("shell", map[string]any{"command": "go build ./..."}),
		toolResult(false, "ok"),
	)
	calls := extractRecentToolCalls(msgs, 12)
	if len(calls) != 1 || calls[0].Tool != "shell" {
		t.Fatalf("expected one call named shell, got %+v", calls)
	}
}

// YM-131: an edit is identified by its path, not by its payload. Two edits of
// the same file with different old/new strings are the same loop step; hashing
// the payload made every edit unique, so a write-test loop never matched.
func TestHashToolCall_EditHashesThePathOnly(t *testing.T) {
	first := hashToolCall("edit", map[string]any{"path": "/w/a.go", "oldString": "a", "newString": "b"})
	second := hashToolCall("edit", map[string]any{"path": "/w/a.go", "oldString": "c", "newString": "d"})
	if first != second {
		t.Error("hashToolCall must ignore the edit payload and hash the file path")
	}

	// Same for the write tool: the content is the payload, not the identity.
	first = hashToolCall("write", map[string]any{"path": "/w/a.go", "content": "one"})
	second = hashToolCall("write", map[string]any{"path": "/w/a.go", "content": "two"})
	if first != second {
		t.Error("hashToolCall must ignore the write content and hash the file path")
	}
}

// YM-131: opencode's shell tool is the build tool. isBuildTool only knew
// "Bash", so no edit/test cycle could ever be counted for opencode.
func TestIsBuildTool_OpencodeShellDialect(t *testing.T) {
	for _, tool := range []string{"shell", "bash"} {
		if !isBuildTool(ToolCallEntry{Tool: tool, Command: "go test ./internal/proxy"}) {
			t.Errorf("%s with 'go test' must count as a build tool", tool)
		}
	}
	if isBuildTool(ToolCallEntry{Tool: "execute", Command: "go test ./..."}) {
		t.Error("the Code Mode tool is not a shell tool")
	}
}

// YM-131: end to end, a loop in the opencode dialect must be detected. This
// is the behaviour the audit found missing (zero LOOP: lines for opencode).
func TestDetectLoop_OpencodeIdenticalCycle(t *testing.T) {
	msgs := buildMessages(
		userMsg("run the tests"),
		los36ToolUse("shell", map[string]any{"command": "go test ./..."}), toolResult(true, "FAIL"),
		los36ToolUse("read", map[string]any{"path": "/src/a.go"}), toolResult(false, "package main"),
		los36ToolUse("shell", map[string]any{"command": "go test ./..."}), toolResult(true, "FAIL"),
		los36ToolUse("read", map[string]any{"path": "/src/a.go"}), toolResult(false, "package main"),
	)

	sig := DetectLoop(msgs)
	if sig == nil {
		t.Fatal("expected a loop signal for the repeated opencode shell/read cycle")
	}
	if sig.Type != LoopIdenticalCycle {
		t.Errorf("expected LoopIdenticalCycle, got %v", sig.Type)
	}
}

// YM-131: opencode's shell carries a workdir, so the same command in two
// directories is a different step. Without it in the hash, one `npm test` in
// two packages looked like a repeated identical cycle.
func TestHashToolCall_ShellIncludesWorkdir(t *testing.T) {
	a := hashToolCall("shell", map[string]any{"command": "npm test", "workdir": "/pkg/a"})
	b := hashToolCall("shell", map[string]any{"command": "npm test", "workdir": "/pkg/b"})
	if a == b {
		t.Error("the same command in two directories must not hash equal")
	}

	// Claude Code's Bash has no workdir: its hash stays stable.
	c := hashToolCall("Bash", map[string]any{"command": "npm test"})
	d := hashToolCall("Bash", map[string]any{"command": "npm test"})
	if c != d {
		t.Error("a command without workdir must hash deterministically")
	}
}

// YM-131: three edit/test/error cycles in the opencode dialect must be
// recognized as an edit-test-error loop, with read-only inspection between the
// cycles not breaking the pattern.
func TestDetectEditTestErrorCycle_OpencodeDialect(t *testing.T) {
	var parts []any
	parts = append(parts, userMsg("fix the failing test"))
	for i := 0; i < minEditTestCycles; i++ {
		parts = append(parts,
			los36ToolUse("edit", map[string]any{"path": "/src/main.go", "oldString": "a", "newString": "b"}),
			toolResult(false, "edited"),
			los36ToolUse("shell", map[string]any{"command": "go test ./internal/proxy"}),
			toolResult(true, "FAIL: TestFix undefined"),
			los36ToolUse("read", map[string]any{"path": "/src/main.go"}),
			toolResult(false, "package main"),
		)
	}

	calls := extractRecentToolCalls(buildMessages(parts...), loopWindowSize)
	sig := detectEditTestErrorCycle(calls)
	if sig == nil {
		t.Fatalf("expected an edit-test-error loop, got nil (calls: %+v)", calls)
	}
	if sig.Type != LoopEditTestError {
		t.Errorf("expected LoopEditTestError, got %v", sig.Type)
	}
	if sig.FilePath != "/src/main.go" {
		t.Errorf("expected the edited path in the signal, got %q", sig.FilePath)
	}
}

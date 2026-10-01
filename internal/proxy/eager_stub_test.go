package proxy

import (
	"fmt"
	"strings"
	"testing"
)

func simpleEstimate(s string) int { return len(s) / 4 }

func TestEagerStub_ReadResult(t *testing.T) {
	code := "package proxy\n\nfunc handleMessages() error {\n\treturn nil\n}\n\nfunc forward() error {\n\treturn nil\n}\n"
	code += strings.Repeat("// padding\n", 200)

	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "internal/proxy/proxy.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": code},
		}},
		map[string]any{"role": "assistant", "content": "The file has two functions."},
		map[string]any{"role": "user", "content": "thanks"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)

	msg := result[1].(map[string]any)
	blocks := msg["content"].([]any)
	block := blocks[0].(map[string]any)

	if block["type"] != "tool_result" {
		t.Errorf("type must stay tool_result, got %v", block["type"])
	}
	if block["tool_use_id"] != "t1" {
		t.Errorf("tool_use_id must be preserved, got %v", block["tool_use_id"])
	}

	stub := block["content"].(string)
	if !strings.Contains(stub, "proxy.go") {
		t.Errorf("stub must contain file path, got: %s", stub)
	}
	if !strings.Contains(stub, "handleMessages") {
		t.Errorf("stub must contain function name, got: %s", stub)
	}
	if len(stub) > 500 {
		t.Errorf("stub must be compact, got %d chars", len(stub))
	}
}

func TestEagerStub_SkipSmall(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "small.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "package main"},
		}},
		map[string]any{"role": "assistant", "content": "Tiny file."},
		map[string]any{"role": "user", "content": "next"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)

	block := result[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["content"] != "package main" {
		t.Error("small tool_result must not be stubbed")
	}
}

func TestEagerStub_RunningTurnNotStubbed(t *testing.T) {
	big := strings.Repeat("code\n", 500)
	messages := []any{
		map[string]any{"role": "user", "content": "analyze this"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "big.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
		}},
		map[string]any{"role": "assistant", "content": "Partial analysis."},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t2", "name": "Read",
				"input": map[string]any{"file_path": "big2.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": big},
		}},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)

	for _, idx := range []int{2, 5} {
		block := result[idx].(map[string]any)["content"].([]any)[0].(map[string]any)
		if block["content"] != big {
			t.Errorf("tool_result at %d within a running turn must not be stubbed", idx)
		}
	}
}

func TestEagerStub_SkipFrozenPrefix(t *testing.T) {
	big := strings.Repeat("frozen\n", 500)
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "old.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
		}},
		map[string]any{"role": "assistant", "content": "Old analysis."},
	}

	result := EagerStubToolResults(messages, 3, simpleEstimate)

	block := result[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["content"] != big {
		t.Error("frozen tool_result must not be stubbed")
	}
}

func TestEagerStub_GrepResult(t *testing.T) {
	grep := strings.Repeat("file.go:42: match here\n", 100)
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Grep",
				"input": map[string]any{"pattern": "handleMessages", "path": "internal/proxy/"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": grep},
		}},
		map[string]any{"role": "assistant", "content": "Found in several files."},
		map[string]any{"role": "user", "content": "ok"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)
	stub := result[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)

	if !strings.Contains(stub, "handleMessages") {
		t.Errorf("grep stub must contain pattern, got: %s", stub)
	}
	if !strings.Contains(stub, "101") {
		t.Errorf("grep stub must contain match count, got: %s", stub)
	}
	if len(stub) > 500 {
		t.Errorf("grep stub too large: %d chars", len(stub))
	}
}

func TestEagerStub_BashResult(t *testing.T) {
	bash := "Building...\n" + strings.Repeat("compiling pkg\n", 200) + "Build OK.\n"
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash",
				"input": map[string]any{"command": "go build ./..."}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": bash},
		}},
		map[string]any{"role": "assistant", "content": "Build succeeded."},
		map[string]any{"role": "user", "content": "ok"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)
	stub := result[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)

	if !strings.Contains(stub, "go build") {
		t.Errorf("bash stub must contain command, got: %s", stub)
	}
	if !strings.Contains(stub, "Building") {
		t.Errorf("bash stub must contain head lines, got: %s", stub)
	}
	if !strings.Contains(stub, "Build OK") {
		t.Errorf("bash stub must contain tail lines, got: %s", stub)
	}
}

func TestEagerStub_GlobResult(t *testing.T) {
	var paths []string
	for i := 0; i < 100; i++ {
		paths = append(paths, fmt.Sprintf("internal/proxy/file%d.go", i))
	}
	glob := strings.Join(paths, "\n")
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Glob",
				"input": map[string]any{"pattern": "**/*.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": glob},
		}},
		map[string]any{"role": "assistant", "content": "Found 50 files."},
		map[string]any{"role": "user", "content": "ok"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)
	stub := result[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)

	if !strings.Contains(stub, "**/*.go") {
		t.Errorf("glob stub must contain pattern, got: %s", stub)
	}
	if !strings.Contains(stub, "100") {
		t.Errorf("glob stub must contain count, got: %s", stub)
	}
}

func TestEagerStub_MultipleResults(t *testing.T) {
	big1 := strings.Repeat("a\n", 1500)
	big2 := strings.Repeat("b\n", 1500)
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "a.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big1},
		}},
		map[string]any{"role": "assistant", "content": "Analyzed a.go"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t2", "name": "Read",
				"input": map[string]any{"file_path": "b.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": big2},
		}},
		map[string]any{"role": "assistant", "content": "Analyzed b.go"},
		map[string]any{"role": "user", "content": "next"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)

	for _, idx := range []int{1, 4} {
		stub := result[idx].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
		if len(stub) > 500 {
			t.Errorf("tool_result at %d should be stubbed, got %d chars", idx, len(stub))
		}
	}
}

func TestEagerStub_PreservesLength(t *testing.T) {
	big := strings.Repeat("x\n", 500)
	messages := []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "f.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
		}},
		map[string]any{"role": "assistant", "content": "Done."},
		map[string]any{"role": "user", "content": "thanks"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)

	if len(result) != 5 {
		t.Errorf("message count must not change: got %d, want 5", len(result))
	}
	if result[0].(map[string]any)["content"] != "start" {
		t.Error("non-tool messages must be unchanged")
	}
}

func TestEagerStubMemory_PersistRoundtrip(t *testing.T) {
	store := map[string]string{}
	persist := func(key, value string) { store[key] = value }
	load := func(key string) (string, bool) {
		v, ok := store[key]
		return v, ok
	}

	m1 := NewEagerStubMemory()
	m1.SetPersistFunc(persist)
	m1.RecordStubbed("threadX", "tool-uuid-1")
	m1.RecordStubbed("threadX", "tool-uuid-2")
	m1.RecordStubbed("threadY", "tool-uuid-3")

	if _, ok := store["eagerstub:threadX"]; !ok {
		t.Fatalf("threadX not persisted; keys=%v", keysOf(store))
	}
	if _, ok := store["eagerstub:threadY"]; !ok {
		t.Fatalf("threadY not persisted; keys=%v", keysOf(store))
	}

	m2 := NewEagerStubMemory()
	m2.SetLoadFunc(load)
	if !m2.WasStubbed("threadX", "tool-uuid-1") {
		t.Errorf("expected loaded WasStubbed(threadX, tool-uuid-1) = true")
	}
	if !m2.WasStubbed("threadX", "tool-uuid-2") {
		t.Errorf("expected loaded WasStubbed(threadX, tool-uuid-2) = true")
	}
	if !m2.WasStubbed("threadY", "tool-uuid-3") {
		t.Errorf("expected loaded WasStubbed(threadY, tool-uuid-3) = true")
	}
	if m2.WasStubbed("threadX", "tool-uuid-3") {
		t.Errorf("threads must be isolated: tool-uuid-3 belongs to threadY only")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEagerStub_StubDecisionPersistsAcrossCalls(t *testing.T) {
	big := strings.Repeat("payload\n", 500)
	threadID := "thread-X"
	toolUseID := "t1"

	memory := NewEagerStubMemory()

	msgs1 := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": toolUseID, "name": "Read",
				"input": map[string]any{"file_path": "x.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": big},
		}},
		map[string]any{"role": "assistant", "content": "Analyzed."},
		map[string]any{"role": "user", "content": "next"},
	}

	r1 := EagerStubToolResults(msgs1, 0, simpleEstimate, WithStubMemory(memory, threadID))
	c1 := r1[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if c1 == big {
		t.Fatalf("Turn 1: expected stubbed, got full text")
	}

	msgs2 := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": toolUseID, "name": "Read",
				"input": map[string]any{"file_path": "x.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": big},
		}},
	}

	r2 := EagerStubToolResults(msgs2, 0, simpleEstimate, WithStubMemory(memory, threadID))
	c2 := r2[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)

	if c2 == big {
		t.Errorf("Turn 2: tool_use_id=%s was stubbed in Turn 1 — must stay stubbed even without following assistant", toolUseID)
	}
	if c1 != c2 {
		t.Errorf("stub bytes must be deterministic across calls: Turn 1=%q, Turn 2=%q", c1, c2)
	}
}

func TestEagerStub_UnknownTool(t *testing.T) {
	big := strings.Repeat("data\n", 500)
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "WebSearch",
				"input": map[string]any{"query": "golang proxy"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
		}},
		map[string]any{"role": "assistant", "content": "Found results."},
		map[string]any{"role": "user", "content": "ok"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)
	stub := result[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)

	if !strings.Contains(stub, "WebSearch") {
		t.Errorf("unknown tool stub must contain tool name, got: %s", stub)
	}
	if !strings.Contains(stub, "archived") {
		t.Errorf("unknown tool stub must say archived, got: %s", stub)
	}
}

func TestEagerStub_CountersDistinguishMemoryFromFresh(t *testing.T) {
	mem := NewEagerStubMemory()
	tid := "tid-counter"

	makeMessages := func(includeClosingUser bool) []any {
		big := strings.Repeat("data\n", 500)
		msgs := []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "Bash",
					"input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
			}},
		}
		if includeClosingUser {
			msgs = append(msgs, map[string]any{"role": "user", "content": "next"})
		}
		return msgs
	}

	var sticky1, fresh1 int
	_ = EagerStubToolResults(makeMessages(true), 0, simpleEstimate,
		WithStubMemory(mem, tid),
		WithStubCounters(&sticky1, &fresh1))

	if fresh1 != 1 || sticky1 != 0 {
		t.Fatalf("turn 1 expected fresh=1 sticky=0, got fresh=%d sticky=%d", fresh1, sticky1)
	}

	var sticky2, fresh2 int
	_ = EagerStubToolResults(makeMessages(false), 0, simpleEstimate,
		WithStubMemory(mem, tid),
		WithStubCounters(&sticky2, &fresh2))

	if sticky2 != 1 || fresh2 != 0 {
		t.Fatalf("turn 2 expected sticky=1 fresh=0, got sticky=%d fresh=%d", sticky2, fresh2)
	}
}

func TestEagerStub_ParallelToolUsesGetOwnLabels(t *testing.T) {
	code := "package a\n\nfunc aOne() error {\n\treturn nil\n}\n" + strings.Repeat("// a padding\n", 300)
	grep := strings.Repeat("bfile.go:7: boundaryCheck hit\n", 150)

	messages := []any{
		map[string]any{"role": "user", "content": "run both"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "a.go"}},
			map[string]any{"type": "tool_use", "id": "t2", "name": "Grep",
				"input": map[string]any{"pattern": "boundaryCheck", "path": "internal/proxy/"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": code},
			map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": grep},
		}},
		map[string]any{"role": "user", "content": "next"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)

	blocks := result[2].(map[string]any)["content"].([]any)
	stub1 := blocks[0].(map[string]any)["content"].(string)
	stub2 := blocks[1].(map[string]any)["content"].(string)

	if !strings.Contains(stub1, "a.go") || strings.Contains(stub1, "boundaryCheck") {
		t.Errorf("stub for t1 must carry its own tool info (a.go, no grep pattern), got: %s", stub1)
	}
	if !strings.Contains(stub2, "boundaryCheck") || strings.Contains(stub2, "a.go") {
		t.Errorf("stub for t2 must carry its own tool info (boundaryCheck, no a.go), got: %s", stub2)
	}
}

func TestEagerStub_UnknownToolUseID(t *testing.T) {
	big := strings.Repeat("data\n", 600)
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "a.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "unknown-1", "content": big},
		}},
		map[string]any{"role": "user", "content": "next"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)

	stub := result[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(stub, "Read") || strings.Contains(stub, "a.go") {
		t.Errorf("stub for unknown tool_use_id must not adopt a foreign tool name, got: %s", stub)
	}
	if !strings.Contains(stub, "tool_result") || !strings.Contains(stub, "archived") {
		t.Errorf("stub for unknown tool_use_id must use the generic form, got: %s", stub)
	}
}

func TestEagerStub_ClosedTurnStubsBigNotSmall(t *testing.T) {
	big := strings.Repeat("payload\n", 600)
	small := "tiny result"
	messages := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "big.go"}},
			map[string]any{"type": "tool_use", "id": "t2", "name": "Read",
				"input": map[string]any{"file_path": "small.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
			map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": small},
		}},
		map[string]any{"role": "user", "content": "next"},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)

	blocks := result[2].(map[string]any)["content"].([]any)
	if got := blocks[0].(map[string]any)["content"].(string); !strings.Contains(got, "big.go") {
		t.Errorf("big tool_result in closed turn must be stubbed, got: %s", got)
	}
	if got := blocks[1].(map[string]any)["content"]; got != small {
		t.Errorf("small tool_result must stay intact, got: %v", got)
	}
}

func TestEagerStub_StickyInRunningTurn(t *testing.T) {
	big := strings.Repeat("payload\n", 600)
	memory := NewEagerStubMemory()
	threadID := "thread-sticky"

	closed := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "x.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
		}},
		map[string]any{"role": "user", "content": "next"},
	}
	EagerStubToolResults(closed, 0, simpleEstimate, WithStubMemory(memory, threadID))

	running := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "x.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t2", "name": "Read",
				"input": map[string]any{"file_path": "y.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": big},
		}},
	}

	var sticky, fresh int
	result := EagerStubToolResults(running, 0, simpleEstimate,
		WithStubMemory(memory, threadID), WithStubCounters(&sticky, &fresh))

	b1 := result[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if b1 == big {
		t.Error("sticky-marked tool_result must stay stubbed inside a running turn")
	}
	b2 := result[4].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if b2 != big {
		t.Error("fresh tool_result without turn boundary must not be stubbed")
	}
	if sticky != 1 || fresh != 0 {
		t.Errorf("expected sticky=1 fresh=0 in running turn, got sticky=%d fresh=%d", sticky, fresh)
	}
}

func TestEagerStub_TextAndToolResultNotABoundary(t *testing.T) {
	big := strings.Repeat("payload\n", 600)
	messages := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "one.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "note alongside results"},
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t2", "name": "Read",
				"input": map[string]any{"file_path": "two.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": big},
		}},
	}

	result := EagerStubToolResults(messages, 0, simpleEstimate)

	for _, idx := range []int{2, 4} {
		for _, block := range result[idx].(map[string]any)["content"].([]any) {
			b, ok := block.(map[string]any)
			if !ok || b["type"] != "tool_result" {
				continue
			}
			if b["content"] != big {
				t.Errorf("tool_result at %d must not be stubbed: user text+tool_result is not a turn boundary", idx)
			}
		}
	}
}

func TestEagerStub_FrozenBoundaryFreshPart(t *testing.T) {
	big := strings.Repeat("payload\n", 600)

	// Fresh part contains its own closed turn: fresh stubbing applies there,
	// frozen prefix (idx 0-3) is untouched even though it also holds a big result.
	messages := []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "old.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
		}},
		map[string]any{"role": "user", "content": "new prompt"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t2", "name": "Read",
				"input": map[string]any{"file_path": "new.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": big},
		}},
		map[string]any{"role": "user", "content": "next"},
	}

	result := EagerStubToolResults(messages, 4, simpleEstimate)

	frozenBlock := result[2].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if frozenBlock != big {
		t.Error("frozen tool_result must not be stubbed")
	}
	freshStub := result[5].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(freshStub, "new.go") {
		t.Errorf("fresh tool_result in closed turn must be stubbed with its own label, got: %s", freshStub)
	}
}

func TestEagerStub_FrozenBoundaryDoesNotEnableFreshStubbing(t *testing.T) {
	big := strings.Repeat("payload\n", 600)

	// Only boundary is the frozen session start (idx 0); the fresh part is a
	// running turn. A boundary below frozenBoundary must not enable stubbing.
	messages := []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
				"input": map[string]any{"file_path": "old.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t2", "name": "Read",
				"input": map[string]any{"file_path": "new.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": big},
		}},
	}

	result := EagerStubToolResults(messages, 2, simpleEstimate)

	for _, idx := range []int{2, 4} {
		if got := result[idx].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]; got != big {
			t.Errorf("fresh tool_result at %d must not be stubbed by a frozen-prefix boundary", idx)
		}
	}
}

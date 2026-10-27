package proxy

import (
	"testing"
)

// YM-322 (1): the block-shaped tool_result path emitted a message with role
// "tool" and a string body. That role does not exist on the Anthropic wire, and
// the failure flag never reached the tool_result block, so the loop detector and
// the inline reflection saw a successful shell call where the shell had failed.
func TestLos56dBlockToolResultIsAnthropicUserMessageWithIsError(t *testing.T) {
	msgs := []OpenAIMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "bash", "input": map[string]any{"command": "go build ./..."}},
		}},
		{Role: "user", Content: []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "compiling\nExited with code 1"},
		}},
	}

	got, err := translateMessages(msgs)
	if err != nil {
		t.Fatalf("translateMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("messages = %d, want 2", len(got))
	}

	last, _ := got[1].(map[string]any)
	if last["role"] != "user" {
		t.Errorf("tool result message role = %v, want user (tool is not an Anthropic role)", last["role"])
	}
	blocks, _ := last["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("content blocks = %d, want 1: %#v", len(blocks), last["content"])
	}
	blk, _ := blocks[0].(map[string]any)
	if blk["type"] != "tool_result" {
		t.Fatalf("block type = %v, want tool_result", blk["type"])
	}
	if blk["tool_use_id"] != "t1" {
		t.Errorf("tool_use_id = %v, want t1", blk["tool_use_id"])
	}
	if blk["is_error"] != true {
		t.Errorf("is_error = %v, want true: a failed shell notice must survive the translation", blk["is_error"])
	}
}

// YM-322 (2): the loop lookahead attributed every error of a message to every
// tool call of the same message. opencode batches parallel tool calls, so one
// failing shell command marked its sibling read as failed too and
// minRepeatedErrors fired on calls that never failed.
func TestLos56dLoopLookaheadMatchesResultByToolUseID(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "call-a", "name": "Bash", "input": map[string]any{"command": "go build ./..."}},
			map[string]any{"type": "tool_use", "id": "call-b", "name": "Read", "input": map[string]any{"file_path": "/tmp/x/y.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call-a", "content": "compiling\nExited with code 1", "is_error": true},
			map[string]any{"type": "tool_result", "tool_use_id": "call-b", "content": "package x"},
		}},
	}

	calls := extractRecentToolCalls(messages, 10)
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if !calls[0].IsError {
		t.Error("shell call with a failed result is not marked as error")
	}
	if calls[1].IsError {
		t.Errorf("read call call-b inherited the shell error of call-a: %q", calls[1].ErrorMsg)
	}
}

// YM-322 (4): the Responses path dropped the failure signal entirely, so a
// codex client's failed shell call never reached the error-driven detection.
func TestLos56dResponsesToolOutputCarriesIsError(t *testing.T) {
	req := map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "run the build"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "compiling\nExited with code 1"},
			map[string]any{"type": "function_call", "call_id": "c2", "name": "read", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c2", "output": "log line\nExited with code 1"},
		},
	}

	anth, err := translateResponsesToAnthropic(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	messages, _ := anth["messages"].([]any)
	var shellResult, readResult map[string]any
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			blk, _ := b.(map[string]any)
			if blk["type"] != "tool_result" {
				continue
			}
			switch blk["tool_use_id"] {
			case "c1":
				shellResult = blk
			case "c2":
				readResult = blk
			}
		}
	}
	if shellResult == nil || readResult == nil {
		t.Fatalf("tool_result blocks missing: %#v", messages)
	}
	if shellResult["is_error"] != true {
		t.Errorf("failed shell output carries no is_error: %#v", shellResult)
	}
	if readResult["is_error"] == true {
		t.Errorf("a read whose output merely mentions the notice is marked as error: %#v", readResult)
	}
}

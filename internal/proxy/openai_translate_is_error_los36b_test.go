package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// los36bToolResultBlocks returns every tool_result block of a translated
// request, flattened across the user messages that carry them.
func los36bToolResultBlocks(t *testing.T, translated []any) []map[string]any {
	t.Helper()
	var blocks []map[string]any
	for _, m := range translated {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		// The opencode tool_result block shape emits the block as a message of
		// its own, the role "tool" shape nests it in a user message.
		if _, ok := msg["tool_use_id"]; ok {
			blocks = append(blocks, msg)
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range content {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "tool_result" {
				blocks = append(blocks, block)
			}
		}
	}
	return blocks
}

// los36bToolMessageRequest decodes a request whose tool answer is what opencode
// puts on the wire: role "tool", a content string, no error flag.
func los36bToolMessageRequest(t *testing.T, result string) OpenAIChatRequest {
	t.Helper()
	body := `{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"go build ./...\"}"}}]},` +
		`{"role":"tool","tool_call_id":"t1","content":` + los36bJSONString(t, result) + `}]}`
	var req OpenAIChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

func los36bJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func los36bIsError(t *testing.T, translated []any) bool {
	t.Helper()
	blocks := los36bToolResultBlocks(t, translated)
	if len(blocks) != 1 {
		t.Fatalf("expected one tool_result block, got %d", len(blocks))
	}
	flag, _ := blocks[0]["is_error"].(bool)
	return flag
}

// opencode's shell tool appends its own notice as the last content part
// (packages/core/src/shell/result.ts:30-34, tool/plugin/shell.ts:79-83). The
// notice is the only error signal that survives the transport, and the loop
// detector reads it through is_error.
func TestTranslateMessages_OpencodeShellErrorSetsIsError(t *testing.T) {
	cases := []struct {
		name   string
		result string
		want   bool
	}{
		{"failed build", "main.go:3: undefined: x\n\nExited with code 1", true},
		{"failed exit code 2", "usage error\n\nExited with code 2", true},
		{"timeout", "still running\n\nTimed out before completion", true},
		{"killed", "partial output\n\nKilled by SIGKILL", true},
		{"successful build", "ok  \tgithub.com/x/y\t0.4s", false},
		{"no notice at all", "ok", false},
		{"zero exit notice is not a failure", "ok\n\nExited with code 0", false},
		{"trailing blank lines after the notice", "boom\n\nExited with code 1\n\n", true},
		{"notice mentioned in the middle is not a failure", "we saw \"Exited with code 1\" earlier\n\nall good now", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := los36bToolMessageRequest(t, tc.result)
			translated, err := translateMessages(req.Messages)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if got := los36bIsError(t, translated); got != tc.want {
				t.Errorf("is_error = %v, want %v for content %q", got, tc.want, tc.result)
			}
		})
	}
}

// A transport that does send the flag itself must keep it: the translator is a
// mapper, not an interpreter, and a dropped flag is the whole bug.
func TestTranslateMessages_IsErrorFlagPassesThrough(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"tool","tool_call_id":"t1","content":"","is_error":true}]}`
	var req OpenAIChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	translated, err := translateMessages(req.Messages)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !los36bIsError(t, translated) {
		t.Error("an explicit is_error on a tool message must reach the tool_result block")
	}
}

// The notice vocabulary belongs to the shell tool. A read whose output happens
// to end with a notice line is not a failure.
func TestTranslateMessages_NoticeOnlyCountsForTheShell(t *testing.T) {
	build := func(toolName, result string) []any {
		body := `{"model":"m","messages":[` +
			`{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"` + toolName + `","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"t1","content":` + los36bJSONString(t, result) + `}]}`
		var req OpenAIChatRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		translated, err := translateMessages(req.Messages)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		return translated
	}

	if !los36bIsError(t, build("shell", "boom\n\nExited with code 1")) {
		t.Error("a failing shell must be an error")
	}
	if !los36bIsError(t, build("Bash", "boom\n\nExited with code 1")) {
		t.Error("a failing Claude Code shell must be an error too")
	}
	if los36bIsError(t, build("read", "log tail\n\nExited with code 1")) {
		t.Error("a read whose log ends with a notice line must not count as a failure")
	}
	if los36bIsError(t, build("grep", "Exited with code 1")) {
		t.Error("a grep result must not count as a failure")
	}
}

// The success path must stay byte-identical: an added "is_error": false would
// change the translated prefix of every request and break the prompt cache.
func TestTranslateMessages_NoIsErrorKeyOnSuccess(t *testing.T) {
	req := los36bToolMessageRequest(t, "ok")
	translated, err := translateMessages(req.Messages)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	blocks := los36bToolResultBlocks(t, translated)
	if len(blocks) != 1 {
		t.Fatalf("expected one tool_result block, got %d", len(blocks))
	}
	if _, present := blocks[0]["is_error"]; present {
		t.Error("a successful tool result must not carry an is_error key at all")
	}
}

// The point of the whole fix: with the flag set, the error-based loop signals
// are reachable for opencode. Different commands per step keep the
// identical-cycle signal out of the way, so the repeated-error signal is what
// has to fire.
func TestDetectLoop_OpencodeBuildErrorsThroughTranslator(t *testing.T) {
	cmds := []string{"go build ./a", "go test ./b", "make check"}
	var msgs []any
	for i, cmd := range cmds {
		args, err := json.Marshal(map[string]any{"command": cmd})
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		id := "t" + string(rune('1'+i))
		body := `{"model":"m","messages":[` +
			`{"role":"assistant","content":"","tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"shell","arguments":` + los36bJSONString(t, string(args)) + `}}]},` +
			`{"role":"tool","tool_call_id":"` + id + `","content":"undefined: x\n\nExited with code 1"}]}`
		var req OpenAIChatRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		translated, err := translateMessages(req.Messages)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		msgs = append(msgs, translated...)
	}

	sig := DetectLoop(msgs)
	if sig == nil {
		t.Fatal("three failing opencode builds must raise a loop signal, got nil")
	}
	if sig.Type != LoopRepeatedError {
		t.Errorf("expected the repeated-error signal, got %q (%s)", sig.Type, sig.Description)
	}
	if !strings.Contains(sig.Description, "Exited with code 1") {
		t.Errorf("the signal must quote the failure, got %q", sig.Description)
	}
}

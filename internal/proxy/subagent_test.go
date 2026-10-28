package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// === Subagent Detection ===

// Helper: build a request map with thinking field (main session)
func mainSessionReq() map[string]any {
	return map[string]any{
		"model":    "claude-opus-4-6",
		"thinking": map[string]any{"type": "adaptive"},
		"system": []any{
			map[string]any{"text": "x-anthropic-billing-header: cc_version=2.1.88.cfc; cc_entrypoint=cli; cch=00000;"},
		},
	}
}

// All cases go through isSubagentRequest, the function the handlers use, so a
// client without a billing header and without thinking is tested as what it is:
// a main session (see sawtooth_los26b_test.go, YM-263).

func TestIsSubagentRequest_NoThinkingNoMarker(t *testing.T) {
	msgs := buildLargeMessages(30)
	req := map[string]any{"model": "claude-opus-4-6", "temperature": 1.0}
	if !isSubagentRequest(http.Header{}, msgs, req) {
		t.Error("unmarked request without thinking should be detected as subagent")
	}
}

func TestIsSubagentRequest_WithThinking(t *testing.T) {
	msgs := buildLargeMessages(1)
	if isSubagentRequest(http.Header{}, msgs, mainSessionReq()) {
		t.Error("request with thinking field should NOT be detected as subagent")
	}
}

func TestIsSubagentRequest_SdkTsEntrypoint(t *testing.T) {
	msgs := buildLargeMessages(5)
	req := map[string]any{
		"thinking": map[string]any{"type": "adaptive"},
		"system": []any{
			map[string]any{"text": "x-anthropic-billing-header: cc_entrypoint=sdk-ts;"},
		},
	}
	if !isSubagentRequest(http.Header{}, msgs, req) {
		t.Error("sdk-ts entrypoint should be detected as subagent regardless of thinking")
	}
}

func TestIsSubagentRequest_HaikuModel(t *testing.T) {
	msgs := buildLargeMessages(5)
	req := map[string]any{
		"model": "claude-haiku-4-5-20251001",
		"system": []any{
			map[string]any{"text": "x-anthropic-billing-header: cc_entrypoint=cli;"},
		},
	}
	if !isSubagentRequest(http.Header{}, msgs, req) {
		t.Error("haiku model should be detected as subagent")
	}
}

func TestIsSubagentRequest_CLIWithThinking(t *testing.T) {
	msgs := buildLargeMessages(200)
	req := map[string]any{
		"thinking": map[string]any{"type": "adaptive"},
		"system": []any{
			map[string]any{"text": "x-anthropic-billing-header: cc_entrypoint=cli;"},
		},
	}
	if isSubagentRequest(http.Header{}, msgs, req) {
		t.Error("CLI session with thinking should NOT be subagent")
	}
}

func TestIsSubagentRequest_EmptyMessages(t *testing.T) {
	if isSubagentRequest(http.Header{}, nil, nil) {
		t.Error("nil messages should not be subagent")
	}
	if isSubagentRequest(http.Header{}, []any{}, mainSessionReq()) {
		t.Error("empty messages should not be subagent")
	}
}

// === Subagent DocsHint Injection ===

func TestInjectDocsHintForSubagent(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "Implement the feature"},
		map[string]any{"role": "assistant", "content": "ok"},
	}
	hint := "[Docs available] Reference docs indexed:\n  twig 3.x, go-stdlib\n[/Docs available]"

	result := injectDocsHintForSubagent(msgs, hint)

	if len(result) != len(msgs)+1 {
		t.Fatalf("expected %d messages, got %d", len(msgs)+1, len(result))
	}
	// The hint should be injected as a user message at the end
	last, ok := result[len(result)-1].(map[string]any)
	if !ok {
		t.Fatal("last message should be a map")
	}
	content, _ := last["content"].(string)
	if !strings.Contains(content, "Docs available") {
		t.Errorf("injected message should contain docs hint, got: %s", content)
	}
}

func TestInjectDocsHintForSubagent_EmptyHint(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "Implement the feature"},
	}

	result := injectDocsHintForSubagent(msgs, "")

	if len(result) != len(msgs) {
		t.Error("empty hint should not add messages")
	}
}

func TestInjectDocsHintForSubagent_NilMessages(t *testing.T) {
	result := injectDocsHintForSubagent(nil, "some hint")
	if result != nil {
		t.Error("nil messages should return nil")
	}
}

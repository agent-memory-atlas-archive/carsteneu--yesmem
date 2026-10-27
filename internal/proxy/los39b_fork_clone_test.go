package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// forkCloneFor muss beide Wire-Formate des OpenAI-Pfads lesen koennen: den
// Chat-Completions-Body unveraendert und den Responses-Body als uebersetzte
// Chat-Completions-Form (YM-262).
func TestForkCloneFor(t *testing.T) {
	chat := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body, format, err := forkCloneFor(chat)
	if err != nil {
		t.Fatalf("chat-completions body: %v", err)
	}
	if format != "openai" || string(body) != string(chat) {
		t.Errorf("chat-completions body must pass through unchanged, got format %q body %s", format, body)
	}

	// The shape the adapter actually forwards: input items without an item type.
	responses := []byte(`{"model":"gpt-5.4","instructions":"Be helpful","input":[{"role":"user","content":"Hello"}],"stream":true,"max_output_tokens":128}`)
	body, format, err = forkCloneFor(responses)
	if err != nil {
		t.Fatalf("responses body: %v", err)
	}
	if format != "openai" {
		t.Fatalf("format = %q, want openai", format)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("translated body is not JSON: %v (%s)", err, body)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("translated body has no messages: %s", body)
	}
	if _, ok := got["input"]; ok {
		t.Errorf("translated body kept the Responses input field: %s", body)
	}
	if got["stream"] != false {
		t.Errorf("translated fork body must not stream, usage arrives in one body: %s", body)
	}
	if !strings.Contains(string(body), "Hello") {
		t.Errorf("translated body lost the user text: %s", body)
	}

	if _, _, err := forkCloneFor([]byte(`{"model":"x"}`)); err == nil {
		t.Error("a body with neither messages nor input must fail")
	}
}

// The translated clone carries the client's cap. The builder must replace it with
// the fork budget, or a 3072-token extraction runs on 32000 tokens (YM-262).
func TestForkCloneThenBuilder_AppliesForkBudget(t *testing.T) {
	responses := []byte(`{"model":"gpt-5.4","instructions":"Be helpful","input":[{"role":"user","content":"Hello"}],"stream":true,"max_output_tokens":32000}`)
	clone, format, err := forkCloneFor(responses)
	if err != nil {
		t.Fatalf("forkCloneFor: %v", err)
	}
	cfg := ForkConfig{Name: "los39b_budget", MaxTokens: 3072, Prompt: func(ForkContext) string { return "[FORK]" }}
	built, err := buildForkRequest(ForkContext{OriginalBody: clone, SourceFormat: format}, cfg)
	if err != nil {
		t.Fatalf("buildForkRequest: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(built, &got); err != nil {
		t.Fatalf("fork body is not JSON: %v (%s)", err, built)
	}
	if got["max_completion_tokens"] != float64(3072) {
		t.Errorf("fork budget = %v, want 3072", got["max_completion_tokens"])
	}
	if !strings.Contains(string(built), "[FORK]") {
		t.Errorf("fork prompt missing: %s", built)
	}
}

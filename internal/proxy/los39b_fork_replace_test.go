package proxy

import (
	"encoding/json"
	"testing"
)

// YM-262 Befund 3: buildForkRequestOpenAI tauschte die messages per bytes.Replace
// an der ERSTEN Fundstelle. Traegt der Body vor "messages" ein Feld mit demselben
// Array (etwa ein vom Client gespiegeltes Echo), landete der Fork-Prompt dort und
// die eigentlichen messages blieben unveraendert.
func TestBuildForkRequestOpenAI_ReplacesOnlyTheMessagesField(t *testing.T) {
	const msgs = `[{"role":"user","content":"Hello"}]`
	body := []byte(`{"model":"deepseek-v4-pro","echo":` + msgs + `,"messages":` + msgs + `}`)

	cfg := ForkConfig{
		Name:      "los39b_span",
		MaxTokens: 3072,
		Prompt:    func(ForkContext) string { return "[FORK]" },
	}
	result, err := buildForkRequest(ForkContext{OriginalBody: body, SourceFormat: "openai"}, cfg)
	if err != nil {
		t.Fatalf("buildForkRequest: %v", err)
	}

	var got struct {
		Echo     []struct{ Content string }
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if len(got.Messages) != 2 {
		t.Fatalf("messages must gain the fork prompt, got %d entries: %s", len(got.Messages), result)
	}
	last := got.Messages[len(got.Messages)-1]
	if last.Role != "user" || last.Content != "[FORK]" {
		t.Errorf("last message = %+v, want the fork prompt", last)
	}
	if len(got.Echo) != 1 {
		t.Fatalf("a sibling field carrying the same array must stay untouched, got %d entries: %s", len(got.Echo), result)
	}
	if got.Echo[0].Content != "Hello" {
		t.Errorf("echo was rewritten: %+v", got.Echo[0])
	}
}

// YM-262: bei doppeltem Top-Level-Key liest der Decoder den letzten Wert, der
// Span-Ersatz patcht den ersten. Statt einen Fork ohne Prompt zu verschicken,
// wird der Body abgelehnt.
func TestBuildForkRequestOpenAI_RefusesDuplicateMessages(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"first"}],"model":"deepseek-v4-pro","messages":[{"role":"user","content":"second"}]}`)
	cfg := ForkConfig{Name: "los39b_dup", MaxTokens: 64, Prompt: func(ForkContext) string { return "[FORK]" }}

	if _, err := buildForkRequest(ForkContext{OriginalBody: body, SourceFormat: "openai"}, cfg); err == nil {
		t.Fatal("a body with a duplicate top-level messages field must be refused")
	}
}

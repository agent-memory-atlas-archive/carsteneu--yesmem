package proxy

import (
	"encoding/json"
	"strconv"
	"testing"
)

// YM-326 Befund 3: der Chat-Completions-Fork uebernahm den max_tokens-Deckel des
// Clients und der Builder ersetzte nur max_completion_tokens. Ein kleiner
// Client-Deckel hat damit die Extraktion abgeschnitten, cfg.MaxTokens konnte ihn
// nicht anheben. Ein Deckel oberhalb des Fork-Budgets bleibt, weil er fuer den
// Fork ausreicht und das Feld in der Regel vor "messages" steht.
func TestLos39c_ChatForkAppliesMaxTokens(t *testing.T) {
	build := func(maxTokens int) map[string]any {
		t.Helper()
		body := []byte(`{"model":"deepseek-chat","max_tokens":` + strconv.Itoa(maxTokens) +
			`,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
		ctx := ForkContext{OriginalBody: body, SourceFormat: "openai"}
		cfg := ForkConfig{Name: "los39c_budget", MaxTokens: 3072, Prompt: func(ForkContext) string { return "[FORK]" }}
		built, err := buildForkRequest(ctx, cfg)
		if err != nil {
			t.Fatalf("buildForkRequest: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(built, &got); err != nil {
			t.Fatalf("fork body is not JSON: %v (%s)", err, built)
		}
		return got
	}

	t.Run("small inherited cap is lifted", func(t *testing.T) {
		if got := build(512); got["max_tokens"] != float64(3072) {
			t.Errorf("fork budget = %v, want 3072 (a 512-token client cap must not truncate the fork)", got["max_tokens"])
		}
	})

	t.Run("larger inherited cap stays", func(t *testing.T) {
		if got := build(32000); got["max_tokens"] != float64(32000) {
			t.Errorf("fork budget = %v, want the inherited 32000 (no prefix break without a reason)", got["max_tokens"])
		}
	})
}

// YM-326 Befund 2: ein forked_agents.model aus der anderen Modellfamilie landete
// im Anthropic-Body und wurde an den Anthropic-Endpoint geschickt (400). Der
// Fork muss beim Modell des Quell-Requests bleiben; die Wire-Familie des Bodys
// ist die Grenze, eine Uebersetzung waere die echte Loesung und ist hier nicht
// der Auftrag.
func TestLos39c_AnthropicForkKeepsOwnModelForForeignFamily(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":8192,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`)
	ctx := ForkContext{OriginalBody: body, SourceFormat: "anthropic"}

	build := func(model string) map[string]any {
		t.Helper()
		cfg := ForkConfig{Name: "los39c_family", Model: model, MaxTokens: 1024, Prompt: func(ForkContext) string { return "[FORK]" }}
		built, err := buildForkRequest(ctx, cfg)
		if err != nil {
			t.Fatalf("buildForkRequest(%q): %v", model, err)
		}
		var got map[string]any
		if err := json.Unmarshal(built, &got); err != nil {
			t.Fatalf("fork body is not JSON: %v (%s)", err, built)
		}
		return got
	}

	if got := build("deepseek-chat"); got["model"] != "claude-opus-5-5" {
		t.Errorf("body model = %v, want the cloned model (an OpenAI-family name does not belong in an Anthropic body)", got["model"])
	}
	if got := build("claude-sonnet-4-6"); got["model"] != "claude-sonnet-4-6" {
		t.Errorf("body model = %v, want the configured Anthropic model", got["model"])
	}
}

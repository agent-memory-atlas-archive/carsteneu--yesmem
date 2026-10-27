package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/sanitize"
)

// llm_complete is the target of the llm() primitive, and its client was the one
// LLM handle in the daemon that reached the provider without a redactor: the
// prompt is caller-supplied, so it is redacted at this boundary rather than
// relying on how the client was wired upstream.
func TestHandleLLMComplete_RedactsThePromptBeforeTheModelSeesIt(t *testing.T) {
	h, _ := mustHandler(t)
	h.redactor = sanitize.NewSecretRedactor(nil)
	h.SetLLMRouting("stub", "", "", nil)

	secret := "AKIAIOSFODNN7EXAMPLE"
	var seen string
	h.SetQualityClient(&spySummarizeClient{
		completeFunc: func(system, userMsg string, opts ...extraction.CallOption) (string, error) {
			seen = system + "\n" + userMsg
			return "ok", nil
		},
	})

	resp := h.handleLLMComplete(map[string]any{
		"system": "you answer plainly",
		"prompt": "the key is " + secret,
	})
	if resp.Error != "" {
		t.Fatalf("llm_complete: %s", resp.Error)
	}
	if strings.Contains(seen, secret) {
		t.Errorf("the prompt reached the model unredacted: %q", seen)
	}
}

// A model override is the one llm_complete client that is built per call. It has
// to come from the daemon's metered factory — a bare client built here would sit
// outside the budget, the api gate and the per-call cap.
func TestHandleLLMComplete_ModelOverrideComesFromTheMeteredFactory(t *testing.T) {
	h, _ := mustHandler(t)

	var gotProvider, gotModel string
	override := func(provider, model string) (extraction.LLMClient, error) {
		gotProvider, gotModel = provider, model
		return &spySummarizeClient{
			completeFunc: func(system, userMsg string, opts ...extraction.CallOption) (string, error) {
				return "override answer", nil
			},
		}, nil
	}
	h.SetLLMRouting("stub", "", "", override)
	h.SetQualityClient(&spySummarizeClient{
		completeFunc: func(system, userMsg string, opts ...extraction.CallOption) (string, error) {
			return "default answer", nil
		},
	})

	resp := h.handleLLMComplete(map[string]any{
		"system": "you answer plainly",
		"prompt": "hello",
		"model":  "special-model",
	})
	if resp.Error != "" {
		t.Fatalf("llm_complete: %s", resp.Error)
	}
	if gotModel != "special-model" {
		t.Errorf("the override factory got model %q, want %q", gotModel, "special-model")
	}
	if gotProvider != "stub" {
		t.Errorf("the override factory got provider %q, want %q", gotProvider, "stub")
	}
	result, _ := llmCompleteResult(t, resp)
	if result != "override answer" {
		t.Errorf("the override client did not answer, got %q", result)
	}
}

// Without a wired factory the override is skipped and the default client
// answers — better a labelled fallback than an unmetered client.
func TestHandleLLMComplete_ModelOverrideSkippedWithoutFactory(t *testing.T) {
	h, _ := mustHandler(t)
	h.SetLLMRouting("stub", "", "", nil)
	h.SetQualityClient(&spySummarizeClient{
		completeFunc: func(system, userMsg string, opts ...extraction.CallOption) (string, error) {
			return "default answer", nil
		},
	})

	resp := h.handleLLMComplete(map[string]any{
		"system": "you answer plainly",
		"prompt": "hello",
		"model":  "special-model",
	})
	if resp.Error != "" {
		t.Fatalf("llm_complete: %s", resp.Error)
	}
	result, _ := llmCompleteResult(t, resp)
	if result != "default answer" {
		t.Errorf("expected the default client to answer, got %q", result)
	}
}

// llmCompleteResult decodes the "result" field of an llm_complete response.
func llmCompleteResult(t *testing.T, resp Response) (string, map[string]any) {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode llm_complete result: %v (raw: %s)", err, resp.Result)
	}
	s, _ := out["result"].(string)
	return s, out
}

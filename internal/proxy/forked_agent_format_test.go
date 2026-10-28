package proxy

import (
	"bytes"
	"encoding/json"
	"testing"
)

// YM-135: the fork builder for OpenAI-format endpoints never normalized the
// effort level. The Anthropic builder forces "high" because providers reject the
// "xhigh" that clients send; the DeepSeek/OpenAI body inherited "xhigh" verbatim
// and the fork call failed with 400 "does not support effort level 'xhigh'".
func TestBuildForkRequestOpenAI_NormalizesInheritedEffort(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-pro","max_tokens":8192,"stream":true,"reasoning_effort":"xhigh","messages":[{"role":"user","content":"Hello"}]}`)

	cfg := ForkConfig{
		Name:      "los39_effort",
		MaxTokens: 3072,
		Prompt:    func(ForkContext) string { return "[FORK]" },
	}
	ctx := ForkContext{OriginalBody: body, SourceFormat: "openai"}

	result, err := buildForkRequest(ctx, cfg)
	if err != nil {
		t.Fatalf("buildForkRequest error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["reasoning_effort"] != "high" {
		t.Errorf("fork reasoning_effort = %v, want high (inherited xhigh is rejected by DeepSeek)", got["reasoning_effort"])
	}
	// An absent effort field must stay absent: inventing a field the provider may
	// not know would trade a 400 for another 400.
	if containsJSONKey(result, "output_config") {
		t.Errorf("fork must not gain an output_config block: %s", result)
	}
}

// YM-135: an OpenAI-format body is byte-identical up to "messages" so the fork
// shares the main thread's KV-cache prefix. Effort normalization happens after
// that prefix and must not re-serialize the document.
func TestBuildForkRequestOpenAI_EffortFixKeepsBytePrefix(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-pro","max_tokens":8192,"messages":[{"role":"user","content":"Hello"}],"stream":true,"reasoning_effort":"xhigh"}`)

	cfg := ForkConfig{Name: "los39_prefix", MaxTokens: 3072, Prompt: func(ForkContext) string { return "[FORK]" }}
	result, err := buildForkRequest(ForkContext{OriginalBody: body, SourceFormat: "openai"}, cfg)
	if err != nil {
		t.Fatalf("buildForkRequest error: %v", err)
	}
	origMsg := bytes.Index(body, []byte(`"messages"`))
	forkMsg := bytes.Index(result, []byte(`"messages"`))
	if origMsg < 0 || forkMsg != origMsg {
		t.Fatalf("messages key moved: orig=%d fork=%d", origMsg, forkMsg)
	}
	if string(body[:origMsg]) != string(result[:origMsg]) {
		t.Errorf("cache prefix changed:\norig=%q\nfork=%q", body[:origMsg], result[:origMsg])
	}
}

// YM-135: the fork's wire format must follow the request it was cloned from, not
// the model name. An Anthropic-format body with a DeepSeek model was built by the
// OpenAI builder and posted to /v1/chat/completions, which cannot read it.
func TestBuildForkRequest_UsesSourceFormatNotModelName(t *testing.T) {
	anthropicBody := []byte(`{"model":"deepseek-v4-flash","max_tokens":8192,"stream":true,"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"Hello"}]}`)

	cfg := ForkConfig{Name: "los39_format", MaxTokens: 3072, Prompt: func(ForkContext) string { return "[FORK]" }}

	anth := ForkContext{OriginalBody: anthropicBody, SourceFormat: "anthropic", AssistantResponse: "hi"}
	result, err := buildForkRequest(anth, cfg)
	if err != nil {
		t.Fatalf("buildForkRequest error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["stream"] != false {
		t.Errorf("Anthropic-format source: fork stream = %v, want false (Anthropic builder)", got["stream"])
	}
	if got["max_tokens"] != float64(3072) {
		t.Errorf("Anthropic-format source: fork max_tokens = %v, want 3072 (Anthropic builder)", got["max_tokens"])
	}

	openaiBody := []byte(`{"model":"deepseek-v4-flash","max_tokens":8192,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`)
	oai := ForkContext{OriginalBody: openaiBody, SourceFormat: "openai"}
	result, err = buildForkRequest(oai, cfg)
	if err != nil {
		t.Fatalf("buildForkRequest error: %v", err)
	}
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["stream"] != true {
		t.Errorf("OpenAI-format source: fork stream = %v, want true (prefix-preserving builder)", got["stream"])
	}
}

// YM-135: the same decision drives the endpoint. Both the builder and the
// endpoint selection must ask one function, or the body and the URL drift apart.
// YM-262 removed the explicit override rank: the fork body is always cloned from
// the source request, so an override could only move the endpoint away from the
// body it sends.
func TestForkIsOpenAI_RankOrder(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		model   string
		want    bool
		comment string
	}{
		{"source wins over model name", "anthropic", "deepseek-v4-pro", false, "the audit case"},
		{"openai source stays openai", "openai", "deepseek-v4-pro", true, ""},
		{"no source falls back to model name", "", "deepseek-v4-pro", true, "legacy callers"},
		{"no source and anthropic model", "", "claude-opus-5-5", false, ""},
	}
	for _, c := range cases {
		ctx := ForkContext{SourceFormat: c.source}
		if got := ctx.forkIsOpenAI(c.model); got != c.want {
			t.Errorf("%s: forkIsOpenAI = %v, want %v %s", c.name, got, c.want, c.comment)
		}
	}
}

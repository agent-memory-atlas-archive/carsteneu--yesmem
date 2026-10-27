package extraction

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeOpenAIResponsesURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", defaultOpenAIResponsesURL},
		{"https://example.test", "https://example.test/v1/responses"},
		{"https://example.test/", "https://example.test/v1/responses"},
		{"https://example.test/v1", "https://example.test/v1/responses"},
		{"https://example.test/v1/responses", "https://example.test/v1/responses"},
	}

	for _, tt := range tests {
		if got := normalizeOpenAIResponsesURL(tt.in); got != tt.want {
			t.Errorf("normalizeOpenAIResponsesURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestOpenAIClientCompleteJSON(t *testing.T) {
	var gotAuth string
	var gotPath string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_text":"{\"ok\":true}","usage":{"input_tokens":12,"output_tokens":7}}`))
	}))
	defer srv.Close()

	oldOnUsage := OnUsage
	defer func() { OnUsage = oldOnUsage }()

	var usageModel string
	var usageIn, usageOut int
	OnUsage = func(model string, inputTokens, outputTokens int) {
		usageModel = model
		usageIn = inputTokens
		usageOut = outputTokens
	}

	client := NewOpenAIClient("sk-openai", "gpt-5-mini", srv.URL, "openai")
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ok": map[string]any{"type": "boolean"},
		},
		"required":             []string{"ok"},
		"additionalProperties": false,
	}

	out, err := client.CompleteJSON("system prompt", "user prompt", schema, WithMaxTokens(321))
	if err != nil {
		t.Fatalf("CompleteJSON() error = %v", err)
	}
	if out != `{"ok":true}` {
		t.Fatalf("CompleteJSON() = %q", out)
	}
	if gotAuth != "Bearer sk-openai" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if gotBody["model"] != "gpt-5-mini" {
		t.Fatalf("model = %#v", gotBody["model"])
	}
	if gotBody["instructions"] != "system prompt" {
		t.Fatalf("instructions = %#v", gotBody["instructions"])
	}
	if gotBody["input"] != "user prompt" {
		t.Fatalf("input = %#v", gotBody["input"])
	}
	if gotBody["max_output_tokens"] != float64(321) {
		t.Fatalf("max_output_tokens = %#v", gotBody["max_output_tokens"])
	}
	if gotBody["store"] != false {
		t.Fatalf("store = %#v, want false", gotBody["store"])
	}

	textCfg, ok := gotBody["text"].(map[string]any)
	if !ok {
		t.Fatalf("text config missing: %#v", gotBody["text"])
	}
	format, ok := textCfg["format"].(map[string]any)
	if !ok {
		t.Fatalf("format missing: %#v", textCfg["format"])
	}
	if format["type"] != "json_schema" {
		t.Fatalf("format.type = %#v", format["type"])
	}
	if format["name"] != "yesmem_output" {
		t.Fatalf("format.name = %#v", format["name"])
	}
	if format["strict"] != true {
		t.Fatalf("format.strict = %#v", format["strict"])
	}

	if usageModel != "gpt-5-mini" || usageIn != 12 || usageOut != 7 {
		t.Fatalf("usage callback = (%q, %d, %d)", usageModel, usageIn, usageOut)
	}
}

func TestOpenAIClientFallsBackToOutputParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"output_text":"",
			"output":[
				{"type":"message","content":[
					{"type":"output_text","text":"first"},
					{"type":"output_text","text":"second"}
				]}
			]
		}`))
	}))
	defer srv.Close()

	client := NewOpenAIClient("sk-openai", "gpt-5-mini", srv.URL, "openai")
	out, err := client.Complete("system", "user")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if out != "first\nsecond" {
		t.Fatalf("Complete() = %q, want %q", out, "first\nsecond")
	}
}

// YM-92: a Chat Completions response reports prompt_tokens/completion_tokens.
// Reading them as the Responses API's input_tokens/output_tokens yields 0 and
// books a zero-cost call, which is what switched the daily budget off.
func TestOpenAIClientChatCompletionsReportsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"done"}}],
			"usage":{"prompt_tokens":2028,"completion_tokens":17,"total_tokens":2045}
		}`))
	}))
	defer srv.Close()

	oldOnUsage := OnUsage
	defer func() { OnUsage = oldOnUsage }()

	var calls, usageIn, usageOut int
	OnUsage = func(model string, inputTokens, outputTokens int) {
		calls++
		usageIn = inputTokens
		usageOut = outputTokens
	}

	client := NewOpenAIClient("sk-deepseek", "deepseek-v4-flash", srv.URL, "openai_compatible")
	out, err := client.Complete("system prompt", "user prompt")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if out != "done" {
		t.Fatalf("Complete() = %q, want %q", out, "done")
	}
	if calls != 1 {
		t.Fatalf("usage reported %d times, want 1", calls)
	}
	if usageIn != 2028 || usageOut != 17 {
		t.Fatalf("usage = (%d, %d), want (2028, 17)", usageIn, usageOut)
	}
}

// YM-92: gateways that speak the Chat Completions shape sometimes also send the
// Responses-API names. Those must keep working instead of bookkeeping zero.
func TestOpenAIClientChatCompletionsAcceptsResponsesUsageNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"done"}}],
			"usage":{"input_tokens":40,"output_tokens":9}
		}`))
	}))
	defer srv.Close()

	oldOnUsage := OnUsage
	defer func() { OnUsage = oldOnUsage }()

	var usageIn, usageOut int
	OnUsage = func(model string, inputTokens, outputTokens int) {
		usageIn = inputTokens
		usageOut = outputTokens
	}

	client := NewOpenAIClient("sk-deepseek", "deepseek-v4-flash", srv.URL, "openai_compatible")
	if _, err := client.Complete("system prompt", "user prompt"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if usageIn != 40 || usageOut != 9 {
		t.Fatalf("usage = (%d, %d), want (40, 9)", usageIn, usageOut)
	}
}

func TestPricingForGPTModels(t *testing.T) {
	tests := []struct {
		model      string
		inputPerM  float64
		outputPerM float64
	}{
		{"gpt-5-mini", 0.25, 2.0},
		{"gpt-5.2", 1.75, 14.0},
		{"gpt-5.2-codex", 1.75, 14.0},
		{"gpt-5.4", 2.5, 15.0},
	}

	for _, tt := range tests {
		in, out := PricingForModel(tt.model)
		if in != tt.inputPerM || out != tt.outputPerM {
			t.Errorf("PricingForModel(%q) = (%v, %v), want (%v, %v)", tt.model, in, out, tt.inputPerM, tt.outputPerM)
		}
	}
}

// Security review: the usage numbers come from a configurable endpoint. A
// negative count would credit the budget, an absurd one would block every
// later call.
func TestOpenAIChatCompletionsClampsUsage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		usage   string
		wantIn  int
		wantOut int
	}{
		{"negative", `{"prompt_tokens":-9000000,"completion_tokens":-3}`, 0, 0},
		{"absurd", `{"prompt_tokens":99999999999,"completion_tokens":7}`, maxReportedTokens, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":` + tc.usage + `}`))
			}))
			defer srv.Close()

			oldOnUsage := OnUsage
			defer func() { OnUsage = oldOnUsage }()
			var usageIn, usageOut int
			OnUsage = func(model string, inputTokens, outputTokens int) {
				usageIn, usageOut = inputTokens, outputTokens
			}

			client := NewOpenAIClient("sk-x", "deepseek-v4-flash", srv.URL, "openai_compatible")
			if _, err := client.Complete("system", "user"); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if usageIn != tc.wantIn || usageOut != tc.wantOut {
				t.Errorf("usage = (%d, %d), want (%d, %d)", usageIn, usageOut, tc.wantIn, tc.wantOut)
			}
		})
	}
}

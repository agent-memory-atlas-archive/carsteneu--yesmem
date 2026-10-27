package extraction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultOpenAIResponsesURL = "https://api.openai.com/v1/responses"

// OpenAIClient is a dual-mode LLM client supporting both OpenAI's Responses API
// and standard Chat Completions API (for DeepSeek and other compatible providers).
type OpenAIClient struct {
	apiKey             string
	model              string
	endpoint           string
	name               string
	useChatCompletions bool // true for openai_compatible (DeepSeek etc.), false for openai (Responses API)
	httpClient         *http.Client
}

func NewOpenAIClient(apiKey, model, baseURL, name string) *OpenAIClient {
	if name == "" {
		name = "openai"
	}
	cc := name == "openai_compatible"
	return &OpenAIClient{
		apiKey:             apiKey,
		model:              model,
		endpoint:           normalizeOpenAIURL(baseURL, cc),
		name:               name,
		useChatCompletions: cc,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			// A 3xx is not followed: the request carries the API key, so the
			// redirect target would receive a credential the caller never
			// configured for it. The proxy forwarding client refuses redirects
			// for the same reason (YM-339).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *OpenAIClient) Name() string  { return c.name }
func (c *OpenAIClient) Model() string { return c.model }

func (c *OpenAIClient) Complete(system, userMessage string, opts ...CallOption) (string, error) {
	return c.doRequest(system, userMessage, nil, opts...)
}

func (c *OpenAIClient) CompleteJSON(system, userMessage string, schema map[string]any, opts ...CallOption) (string, error) {
	if c.useChatCompletions {
		// Chat Completions: schema is appended to the prompt instructions.
		return c.doChatRequest(system, userMessage, schema, opts...)
	}
	strict := true
	return c.doRequest(system, userMessage, &openAITextConfig{
		Format: &openAIResponseFormat{
			Type:        "json_schema",
			Name:        "yesmem_output",
			Schema:      schema,
			Strict:      &strict,
			Description: "Structured extraction output for yesmem.",
		},
	}, opts...)
}

// ━━━ Chat Completions types ━━━

type openAIChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature *float64            `json:"temperature,omitempty"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	Choices []openAIChatChoice   `json:"choices"`
	Usage   *openAIChatUsage     `json:"usage,omitempty"`
	Error   *openAIErrorEnvelope `json:"error,omitempty"`
}

type openAIChatChoice struct {
	Message openAIChatMessage `json:"message"`
}

// openAIChatUsage is the Chat Completions usage shape. The Responses-API names
// are accepted as a fallback because some gateways send those inside a chat
// body; without the chat names a chat response books zero tokens.
//
// Cache accounting: Chat Completions counts cache hits INSIDE prompt_tokens
// (OpenAI nests the hit count in prompt_tokens_details.cached_tokens, a
// DeepSeek-style gateway reports prompt_cache_hit_tokens next to
// prompt_cache_miss_tokens, a gateway proxying Anthropic passes its own field
// names through). Reading only the totals paid every cache hit at the uncached
// input rate, which is what YM-412 reports.
type openAIChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`

	PromptTokensDetails      *openAIChatPromptDetails `json:"prompt_tokens_details,omitempty"`
	PromptCacheHitTokens     int                      `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    int                      `json:"prompt_cache_miss_tokens"`
	CacheReadInputTokens     int                      `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int                      `json:"cache_creation_input_tokens"`
}

type openAIChatPromptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// cacheRead is the reported cache-hit count and whether the prompt total
// already contains it. OpenAI nests its count inside prompt_tokens and the
// DeepSeek-style counters are parts of it, while the Anthropic field name a
// gateway passes through is exclusive — subtracting that one would count the
// hits twice.
func (u *openAIChatUsage) cacheRead() (n int, included bool) {
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		return clampTokens(u.PromptTokensDetails.CachedTokens), true
	}
	if u.PromptCacheHitTokens > 0 {
		return clampTokens(u.PromptCacheHitTokens), true
	}
	return clampTokens(u.CacheReadInputTokens), false
}

// billable reports the metering counts of this response. Cache hits sit inside
// the prompt totals, so the cached part is taken out before billableInput prices
// it: uncached input and cache writes at the full rate, cache reads at their
// discount.
func (u *openAIChatUsage) billable() (in, out int) {
	cacheRead, included := u.cacheRead()
	cacheWrite := clampTokens(u.CacheCreationInputTokens)

	base := clampTokens(u.PromptTokens)
	if base == 0 {
		// Responses-API names in a chat body.
		base = clampTokens(u.InputTokens)
	}
	raw := base
	if included {
		raw = base - cacheRead - cacheWrite
	}
	if raw < 0 {
		raw = 0
	}
	// A miss counter only ever raises the booked amount. It is a component of the
	// prompt total, so a gateway that reports it in another unit must not be able
	// to talk the budget down.
	if miss := clampTokens(u.PromptCacheMissTokens); miss > raw {
		raw = miss
	}

	out = u.CompletionTokens
	if out == 0 {
		out = u.OutputTokens
	}
	return clampTokens(billableInput(raw, cacheWrite, cacheRead)), clampTokens(out)
}

// maxReportedTokens bounds one call's token count. The numbers come from a
// configurable endpoint; a negative count would credit the budget, and a count
// far beyond any real context window would block every later call for the day.
const maxReportedTokens = 2_000_000

func clampTokens(n int) int {
	if n < 0 {
		return 0
	}
	if n > maxReportedTokens {
		return maxReportedTokens
	}
	return n
}

// ━━━ Responses API types ━━━

type openAIResponsesRequest struct {
	Model           string            `json:"model"`
	Input           any               `json:"input"`
	Instructions    string            `json:"instructions,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	Store           *bool             `json:"store,omitempty"`
	Text            *openAITextConfig `json:"text,omitempty"`
}

type openAITextConfig struct {
	Format *openAIResponseFormat `json:"format,omitempty"`
}

type openAIResponseFormat struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
	Description string         `json:"description,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

type openAIResponsesResponse struct {
	OutputText string                   `json:"output_text"`
	Output     []openAIResponsesMessage `json:"output"`
	Usage      *openAIResponsesUsage    `json:"usage,omitempty"`
	Error      *openAIErrorEnvelope     `json:"error,omitempty"`
}

type openAIResponsesMessage struct {
	Type    string                `json:"type"`
	Content []openAIResponsesPart `json:"content,omitempty"`
}

type openAIResponsesPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type openAIResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// InputTokensDetails carries the cache-hit count; input_tokens includes it.
	InputTokensDetails *openAIInputTokensDetails `json:"input_tokens_details,omitempty"`
}

type openAIInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// billable prices the cache hits at their discount instead of the input rate,
// the same split the Chat Completions path makes (YM-412).
func (u *openAIResponsesUsage) billable() (in, out int) {
	cacheRead := 0
	if u.InputTokensDetails != nil {
		cacheRead = clampTokens(u.InputTokensDetails.CachedTokens)
	}
	raw := clampTokens(u.InputTokens) - cacheRead
	if raw < 0 {
		raw = 0
	}
	return clampTokens(billableInput(raw, 0, cacheRead)), clampTokens(u.OutputTokens)
}

type openAIErrorEnvelope struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ━━━ Request dispatch ━━━

func (c *OpenAIClient) doRequest(system, userMessage string, textCfg *openAITextConfig, opts ...CallOption) (string, error) {
	if c.useChatCompletions {
		return c.doChatRequest(system, userMessage, nil, opts...)
	}

	o := applyOpts(opts)
	store := false
	body := openAIResponsesRequest{
		Model:           c.model,
		Input:           userMessage,
		Instructions:    system,
		MaxOutputTokens: o.maxTokens,
		Store:           &store,
		Text:            textCfg,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", c.endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if redirect := redirectRefusal(resp); redirect != nil {
		return "", redirect
	}

	var apiResp openAIResponsesResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	if resp.StatusCode >= 400 {
		if apiResp.Error != nil {
			return "", fmt.Errorf("api error: %s: %s", apiResp.Error.Type, apiResp.Error.Message)
		}
		return "", fmt.Errorf("api error: http %d", resp.StatusCode)
	}
	if apiResp.Error != nil {
		return "", fmt.Errorf("api error: %s: %s", apiResp.Error.Type, apiResp.Error.Message)
	}

	result := strings.TrimSpace(apiResp.OutputText)
	if result == "" {
		var parts []string
		for _, item := range apiResp.Output {
			if item.Type != "message" {
				continue
			}
			for _, part := range item.Content {
				if part.Type == "output_text" && strings.TrimSpace(part.Text) != "" {
					parts = append(parts, part.Text)
				}
			}
		}
		result = strings.TrimSpace(strings.Join(parts, "\n"))
	}
	if result == "" {
		return "", fmt.Errorf("empty response")
	}

	if apiResp.Usage != nil {
		in, out := apiResp.Usage.billable()
		reportMeasuredUsage(o.usageSink, c.model, in, out)
	}

	return result, nil
}

// doChatRequest sends a Chat Completions API request (for DeepSeek etc.).
func (c *OpenAIClient) doChatRequest(system, userMessage string, schema map[string]any, opts ...CallOption) (string, error) {
	o := applyOpts(opts)

	prompt := userMessage
	if schema != nil {
		schemaJSON, _ := json.Marshal(schema)
		prompt += "\n\nOUTPUT FORMAT: Respond with a JSON object matching this schema. Output ONLY the JSON, no markdown fences:\n" + string(schemaJSON)
	}

	messages := []openAIChatMessage{}
	if system != "" {
		messages = append(messages, openAIChatMessage{Role: "system", Content: system})
	}
	messages = append(messages, openAIChatMessage{Role: "user", Content: prompt})

	temp := 0.0
	reqBody := openAIChatRequest{
		Model:       c.model,
		Messages:    messages,
		MaxTokens:   o.maxTokens,
		Temperature: &temp,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequest("POST", c.endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if o.sessionID != "" {
		req.Header.Set("x-opencode-session", o.sessionID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat api call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read chat response: %w", err)
	}

	if redirect := redirectRefusal(resp); redirect != nil {
		return "", redirect
	}

	var chatResp openAIChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("unmarshal chat response: %w [body=%s]", err, string(respBody[:min(len(respBody), 200)]))
	}

	if resp.StatusCode >= 400 || chatResp.Error != nil {
		if chatResp.Error != nil {
			return "", fmt.Errorf("chat api error: %s: %s", chatResp.Error.Type, chatResp.Error.Message)
		}
		return "", fmt.Errorf("chat api error: http %d", resp.StatusCode)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("chat: empty choices")
	}

	result := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	if result == "" {
		return "", fmt.Errorf("chat: empty response")
	}

	if chatResp.Usage != nil {
		in, out := chatResp.Usage.billable()
		reportMeasuredUsage(o.usageSink, c.model, in, out)
	}

	return result, nil
}

// normalizeOpenAIResponsesURL normalizes a base URL for the Responses API.
// This is a convenience wrapper around normalizeOpenAIURL with useChatCompletions=false.
func normalizeOpenAIResponsesURL(baseURL string) string {
	return normalizeOpenAIURL(baseURL, false)
}

func normalizeOpenAIURL(baseURL string, useChatCompletions bool) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		if useChatCompletions {
			return defaultOpenAIResponsesURL // fallback
		}
		return defaultOpenAIResponsesURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	if useChatCompletions {
		switch {
		case strings.HasSuffix(baseURL, "/chat/completions"):
			return baseURL
		case strings.HasSuffix(baseURL, "/v1"):
			return baseURL + "/chat/completions"
		default:
			return baseURL + "/v1/chat/completions"
		}
	}

	// Responses API
	switch {
	case strings.HasSuffix(baseURL, "/responses"):
		return baseURL
	case strings.HasSuffix(baseURL, "/v1"):
		return baseURL + "/responses"
	default:
		return baseURL + "/v1/responses"
	}
}

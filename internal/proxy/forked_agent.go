package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ForkConfig defines a fork type with its trigger, prompt, and response handler.
type ForkConfig struct {
	Name        string
	Model       string
	MaxTokens   int
	Prompt      func(ForkContext) string
	ParseResult func(ForkResponse, *Server) error
	Gate        func(ForkContext) bool
}

// PreviousForkLearning is a learning from a prior fork in this session.
type PreviousForkLearning struct {
	Content  string
	Category string
}

// ForkContext holds everything the fork needs from the main request/response cycle.
type ForkContext struct {
	OriginalBody      []byte
	AssistantResponse string
	ThreadID          string
	Project           string
	ReqIdx            int
	TokensUsed        int
	CacheReadTokens   int
	MessageCount      int
	ToolUseSummary    []string
	// SourceFormat is the wire format of OriginalBody: "anthropic" or "openai".
	// The fork clones that body, so the clone must go to the matching endpoint;
	// deriving the format from the model name sends an Anthropic body to
	// /v1/chat/completions (YM-135). Empty means unknown, then the model name
	// decides (kept for callers and tests that predate this field).
	SourceFormat          string
	InjectedIDs           map[int64]string // learning ID → source
	LastExtractedIdx      int              // message index of last extraction
	HasTrackedDocReads    bool
	PreviousForkLearnings []PreviousForkLearning
	SessionID             string
	AuthHeader            http.Header // original request auth headers for subscription forwarding
	// SourceTarget is the upstream the client's own request was forwarded to. It
	// is captured at the call site because the body model can be rewritten after
	// routing (a "providerID/modelID" prefix is stripped once the target is
	// resolved), so deriving it from the clone would name a different provider.
	// The fork may only forward the client's credential to that origin (YM-326).
	SourceTarget string
}

// ReflectionUsage holds token counts of a fork API call.
type ReflectionUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
}

// ForkResponse holds the parsed fork response.
type ForkResponse struct {
	Content       string
	Usage         ReflectionUsage
	SessionID     string
	Project       string
	SourceMsgFrom int
	SourceMsgTo   int
}

// isAnthropicFamilyModel reports whether a model name belongs to the Anthropic
// wire format. The question is which family a name comes from, not whether it is
// a DeepSeek name: gpt-5.2, glm-4.6 and friends are perfectly valid in an
// OpenAI-compatible body.
func isAnthropicFamilyModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "claude") || strings.Contains(m, "anthropic")
}

// forkModel returns the model the fork request will carry: the explicit config
// model, otherwise the model of the cloned body.
func (ctx ForkContext) forkModel(cfg ForkConfig) string {
	if cfg.Model != "" {
		return cfg.Model
	}
	return extractModelFromBody(ctx.OriginalBody)
}

// forkIsOpenAI decides which wire format the fork request uses. Rank order: the
// format the cloned body came in, then the model name (only for callers that
// report no source format). An explicit override used to sit above both ranks;
// it was removed because the body is always cloned from the source request, so
// an override could only move endpoint and parser away from the body that is
// actually sent (YM-262).
func (ctx ForkContext) forkIsOpenAI(model string) bool {
	switch ctx.SourceFormat {
	case "openai":
		return true
	case "anthropic":
		return false
	}
	return strings.HasPrefix(strings.ToLower(model), "deepseek")
}

// forkForeignModel reports the configured fork model when it belongs to the other
// wire family than the body the fork clones. Such a name cannot reach the body
// (both builders refuse it), so the fork falls back to the request's own model.
// Reporting it keeps the configuration mismatch visible instead of silently
// ignoring forked_agents.model (YM-326). Refusing the fork outright would switch
// extraction off for every setup whose model family differs from the client's;
// translating the body between families is the real fix and out of scope here.
func (ctx ForkContext) forkForeignModel(cfg ForkConfig) string {
	if cfg.Model == "" {
		return ""
	}
	if ctx.forkIsOpenAI(ctx.forkModel(cfg)) == isAnthropicFamilyModel(cfg.Model) {
		return cfg.Model
	}
	return ""
}

// buildForkRequest clones the original request, appends the fork prompt, and overrides model/stream.
func buildForkRequest(ctx ForkContext, cfg ForkConfig) ([]byte, error) {
	// Determine actual model for this fork
	actualModel := ctx.forkModel(cfg)
	if ctx.forkIsOpenAI(actualModel) {
		return buildForkRequestOpenAI(ctx, cfg)
	}
	return buildForkRequestAnthropic(ctx, cfg)
}

// buildForkRequestOpenAI builds a fork request for DeepSeek/OpenAI.
// Preserves the byte-identical prefix with the main request: only the messages
// value is swapped, so key order and all other fields stay exactly as in the
// original body.
func buildForkRequestOpenAI(ctx ForkContext, cfg ForkConfig) ([]byte, error) {
	// A duplicate top-level key makes the two reads below disagree: the decoder
	// keeps the LAST value while the span replacement patches the FIRST, so the
	// fork would go out without its prompt. Refuse the body instead.
	if dup := jsonTopLevelDuplicateKey(ctx.OriginalBody); dup != "" {
		return nil, fmt.Errorf("duplicate top-level key %q in fork body", dup)
	}

	// Extract original messages as raw bytes
	var tmp struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(ctx.OriginalBody, &tmp); err != nil {
		return nil, fmt.Errorf("unmarshal to extract messages: %w", err)
	}

	// Parse message list
	var msgList []json.RawMessage
	if err := json.Unmarshal(tmp.Messages, &msgList); err != nil {
		return nil, fmt.Errorf("unmarshal messages: %w", err)
	}
	if msgList == nil {
		return nil, fmt.Errorf("no messages in original request")
	}

	// Append fork prompt as user message
	b, err := json.Marshal(map[string]any{
		"role": "user", "content": cfg.Prompt(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal fork prompt: %w", err)
	}
	msgList = append(msgList, json.RawMessage(b))

	newMsgs, err := json.Marshal(msgList)
	if err != nil {
		return nil, fmt.Errorf("marshal messages: %w", err)
	}

	// Replace the messages value in the original body — preserves key order and
	// every other field. The field is located structurally: a byte search would
	// rewrite a sibling that happens to carry the same array first (YM-262).
	result, ok := replaceTopLevelJSONValue(ctx.OriginalBody, "messages", newMsgs)
	if !ok {
		return nil, fmt.Errorf("no messages field in original request")
	}

	// Normalize an inherited effort level to "high", mirroring the Anthropic
	// builder. Clients send "xhigh", which DeepSeek and other providers reject
	// with 400 "does not support effort level 'xhigh'". Only an existing field is
	// rewritten: adding one would change the request shape on endpoints that do
	// not know the field.
	//
	// The replacement edits the field value in place. When "reasoning_effort"
	// sits behind "messages" (the common order) the cache prefix stays
	// byte-identical; when it sits in front of it, this fork loses the cache read
	// once. Correctness wins over the cache hit there, because the alternative is
	// a 400.
	var effortProbe struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if json.Unmarshal(ctx.OriginalBody, &effortProbe) == nil &&
		effortProbe.ReasoningEffort != "" && effortProbe.ReasoningEffort != "high" {
		result = replaceJSONStringValue(result, "reasoning_effort", "high")
	}

	// A configured fork model has to reach the body, or the request names one
	// model while the endpoint was resolved for another. An Anthropic model name
	// stays out: it would just move the 400. Everything else belongs in an
	// OpenAI-compatible body.
	if cfg.Model != "" && !isAnthropicFamilyModel(cfg.Model) {
		// json.Marshal escapes; splicing the raw name could produce invalid JSON.
		if quoted, err := json.Marshal(cfg.Model); err == nil {
			result = replaceJSONStringValue(result, "model", string(quoted[1:len(quoted)-1]))
		}
	}

	// The inherited cap belongs to the client, so the fork's own budget has to
	// reach the body. How far that goes depends on where the cap came from:
	//
	// max_completion_tokens comes from the translated /v1/responses clone, which
	// was built from scratch and shares no prefix, so the budget replaces it
	// unconditionally.
	//
	// A chat-completions body as the client sent it carries max_tokens, usually in
	// front of "messages", where every rewrite breaks the byte-identical prefix
	// the fork relies on. There the budget only lifts a cap that is too small for
	// the fork: a 512-token client cap must not truncate a 3072-token extraction,
	// while a cap above the budget needs no change (YM-326).
	if cfg.MaxTokens > 0 {
		budget := []byte(fmt.Sprintf("%d", cfg.MaxTokens))
		if out, ok := replaceTopLevelJSONValue(result, "max_completion_tokens", budget); ok {
			result = out
		}
		var inherited struct {
			MaxTokens *float64 `json:"max_tokens"`
		}
		if json.Unmarshal(result, &inherited) == nil && inherited.MaxTokens != nil &&
			*inherited.MaxTokens < float64(cfg.MaxTokens) {
			if out, ok := replaceTopLevelJSONValue(result, "max_tokens", budget); ok {
				result = out
			}
		}
	}

	return result, nil
}

// buildForkRequestAnthropic is the original implementation for Anthropic API forks.
// Maintains the existing behavior: strips anti_distillation/context_management,
// overrides max_tokens. Cache prefix preservation is not needed for Anthropic
// which uses explicit cache_control breakpoints, not automatic prefix hashing.
func buildForkRequestAnthropic(ctx ForkContext, cfg ForkConfig) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(ctx.OriginalBody, &req); err != nil {
		return nil, fmt.Errorf("unmarshal original: %w", err)
	}

	// Get existing messages (byte-identical prefix for cache hit)
	messages, _ := req["messages"].([]any)
	if messages == nil {
		return nil, fmt.Errorf("no messages in original request")
	}

	// Append: assistant response from main conversation + fork task prompt
	if ctx.AssistantResponse != "" {
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": ctx.AssistantResponse,
		})
	}
	messages = append(messages, map[string]any{
		"role":    "user",
		"content": cfg.Prompt(ctx),
	})
	req["messages"] = messages

	// Override: model, max_tokens, stream=false (no SSE needed)
	// Empty cfg.Model = keep the model from the original request (same as main thread)
	// A model of the other wire family stays out, exactly as in the OpenAI
	// builder: writing it here would only move the 400 to the upstream (YM-326).
	if cfg.Model != "" && isAnthropicFamilyModel(cfg.Model) {
		req["model"] = cfg.Model
	}
	req["max_tokens"] = cfg.MaxTokens
	req["stream"] = false

	// Keep tools and tool_choice intact — the fork prompt asks for JSON output,
	// not tool calls. Preserving them keeps the cache prefix byte-identical
	// to the main thread request, so Anthropic serves a cache read instead
	// of a full cache write (~$43/day savings at current fork volume).

	// Normalize effort to "high" for fork requests. The original request may
	// carry "xhigh" (e.g. from OpenCode CLI) which DeepSeek and other providers
	// reject with 400: "This model does not support effort level 'xhigh'".
	// "high" is the maximum universally supported level across all providers.
	if oc, ok := req["output_config"].(map[string]any); ok {
		oc["effort"] = "high"
	} else {
		req["output_config"] = map[string]any{"effort": "high"}
	}

	// Strip anti-distillation (fake tools would pollute fork response)
	delete(req, "anti_distillation")
	// Strip context_management — rejected by Anthropic API with 400
	delete(req, "context_management")

	return json.Marshal(req)
}

// forkCloneFor returns the body a fork clones together with the wire format it
// carries. A chat-completions body is used as it is; the /v1/responses adapter
// forwards Responses-shaped JSON ("input" instead of "messages"), which the fork
// builder cannot read, so it is translated to chat completions. The fork then
// keeps the wire family of the main request, which is what makes its endpoint
// and its credentials match the body (YM-262).
func forkCloneFor(body []byte) ([]byte, string, error) {
	var probe struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, "", fmt.Errorf("probe fork body: %w", err)
	}
	if len(probe.Messages) > 0 && string(probe.Messages) != "null" {
		return body, "openai", nil
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", fmt.Errorf("unmarshal fork body: %w", err)
	}
	if _, ok := req["input"]; !ok {
		return nil, "", fmt.Errorf("fork body has neither messages nor input")
	}
	anth, err := translateResponsesToAnthropic(req)
	if err != nil {
		return nil, "", fmt.Errorf("responses to anthropic: %w", err)
	}
	oai, err := translateAnthropicToOpenAI(anth)
	if err != nil {
		return nil, "", fmt.Errorf("anthropic to openai: %w", err)
	}
	// The clone shares no cache prefix with the main request, so nothing argues
	// for keeping a streamed response here. A non-streaming fork delivers its
	// usage in one body; a streamed one only reports it in a final chunk that
	// many targets omit without stream_options (YM-262).
	oai["stream"] = false
	clone, err := json.Marshal(oai)
	if err != nil {
		return nil, "", fmt.Errorf("marshal fork body: %w", err)
	}
	return clone, "openai", nil
}

// openAIForkDelay is the pause before an OpenAI-format fork call. DeepSeek only
// persists its disk cache after the main request completed, so an immediate fork
// reads 0% cache. A variable because tests exercise the call path without paying
// the wait.
var openAIForkDelay = 30 * time.Second

// fireForkedAgents evaluates all fork configs and fires matching ones.
func (s *Server) fireForkedAgents(ctx ForkContext, configs []ForkConfig) {
	// Fetch previous fork learnings for session-aware prompt
	if ctx.SessionID != "" {
		raw, err := s.queryDaemon("get_fork_learnings", map[string]any{"session_id": ctx.SessionID})
		if err == nil && raw != nil {
			var resp struct {
				Learnings []struct {
					Content  string `json:"content"`
					Category string `json:"category"`
				} `json:"learnings"`
			}
			if json.Unmarshal(raw, &resp) == nil {
				for _, l := range resp.Learnings {
					ctx.PreviousForkLearnings = append(ctx.PreviousForkLearnings, PreviousForkLearning{
						Content:  l.Content,
						Category: l.Category,
					})
				}
			}
		}
	}

	debugFork := s.cfg.ForkedAgentsDebug

	for _, cfg := range configs {
		if cfg.Gate != nil && !cfg.Gate(ctx) {
			if debugFork {
				s.logger.Printf("[req %d %s] fork %s: gate blocked (tokens=%d, msgs=%d)",
					ctx.ReqIdx, journalToken(s.version), cfg.Name, ctx.TokensUsed, ctx.MessageCount)
			}
			continue
		}

		if foreign := ctx.forkForeignModel(cfg); foreign != "" {
			if _, warned := s.forkModelWarned.LoadOrStore(cfg.Name+"|"+foreign, struct{}{}); !warned {
				s.logger.Printf("[req %d %s] fork %s: configured model %s does not belong to the cloned body's wire family; the fork uses the request's own model",
					ctx.ReqIdx, journalToken(s.version), cfg.Name, journalToken(foreign))
			}
		}

		reqBody, err := buildForkRequest(ctx, cfg)
		if err != nil {
			s.logger.Printf("[req %d %s] fork %s: build error: %v",
				ctx.ReqIdx, journalToken(s.version), cfg.Name, err)
			s.forkState.RecordFailure(ctx.ThreadID)
			continue
		}

		// The format comes from the source request; the endpoint from the model the
		// built body actually carries. Both are derived from the same inputs, so a
		// request cannot name one model and be routed by another.
		isOpenAI := ctx.forkIsOpenAI(ctx.forkModel(cfg))
		bodyModel := extractModelFromBody(reqBody)
		endpoint := s.resolveAnthropicTarget(bodyModel)
		if isOpenAI {
			endpoint = s.resolveOpenAITarget(bodyModel)
		}

		// The credential decides whether this fork can be sent at all, so it is
		// resolved before the cache delay: a fork that has no credential for its
		// target must not hold a goroutine for half a minute first.
		authHeaders, authKey, usable := s.forkAuthFor(ctx, endpoint, isOpenAI)
		if !usable {
			s.logger.Printf("%s[req %d %s] fork %s: no credential for %s (the client's credential belongs to %s), fork skipped%s",
				colorOrange, ctx.ReqIdx, journalToken(s.version), cfg.Name, forkOriginForLog(endpoint), forkOriginForLog(ctx.SourceTarget), colorReset)
			// ShouldFork left the thread pending: without a release no later
			// request would ever fork again (fork_state.go).
			s.forkState.RecordFailure(ctx.ThreadID)
			continue
		}

		// DeepSeek disk cache takes seconds to persist after request completion.
		// Without a delay the fork fires before the cache unit is written → 0% hit.
		if isOpenAI {
			time.Sleep(openAIForkDelay)
		}

		resp, err := s.doForkCall(endpoint, authKey, authHeaders, reqBody, isOpenAI)
		if err != nil {
			s.logger.Printf("%s[req %d %s] fork %s: API error: %v%s",
				colorOrange, ctx.ReqIdx, journalToken(s.version), cfg.Name, err, colorReset)
			s.forkState.RecordFailure(ctx.ThreadID)
			continue
		}
		resp.SessionID = ctx.SessionID
		resp.Project = ctx.Project
		resp.SourceMsgFrom = 0
		resp.SourceMsgTo = ctx.MessageCount - 1

		s.logger.Printf("[req %d %s] fork %s: %d in / %d cached / %d out tokens",
			ctx.ReqIdx, journalToken(s.version), cfg.Name,
			resp.Usage.InputTokens, resp.Usage.CacheReadInputTokens, resp.Usage.OutputTokens)

		go s.queryDaemon("track_fork_usage", map[string]any{
			"input_tokens":          resp.Usage.InputTokens,
			"output_tokens":         resp.Usage.OutputTokens,
			"cache_read_tokens":     resp.Usage.CacheReadInputTokens,
			"cache_creation_tokens": resp.Usage.CacheCreationInputTokens,
		})

		if cfg.ParseResult != nil {
			if err := cfg.ParseResult(resp, s); err != nil {
				s.logger.Printf("[req %d %s] fork %s: parse error: %v",
					ctx.ReqIdx, journalToken(s.version), cfg.Name, err)
			}
		}

		s.forkState.RecordFork(ctx.ThreadID)
	}
}

// forkAuthFor decides which credential a fork call may carry, and whether the fork
// can be sent at all. "Usable" means the request may go out, not that it is
// authenticated: a target without any credential is legitimate (a local
// OpenAI-compatible server), so it gets the request without an auth header,
// exactly like the client's own request did.
//
// The client's own credential is scoped to the upstream its request was routed
// to, so forwarding it to a different origin would hand one provider's key to
// another. cfg.APIKey belongs to the configured upstream and is an Anthropic
// credential: as a Bearer at an OpenAI-compatible endpoint, or as x-api-key at a
// third-party provider, it is a foreign key that can only fail (YM-326).
func (s *Server) forkAuthFor(ctx ForkContext, endpoint string, isOpenAI bool) (http.Header, string, bool) {
	if forkSameOrigin(endpoint, ctx.SourceTarget) {
		headers := forkHeadersFor(ctx.AuthHeader, true)
		if isOpenAI {
			return headers, "", true
		}
		// The proxy's own key authenticates its configured upstream only. A
		// same-origin fork on a different Anthropic-compatible provider must live
		// on the client's credential, or on none, exactly like the client's own
		// request did.
		if forkSameOrigin(endpoint, s.cfg.TargetURL) {
			return headers, s.cfg.APIKey, true
		}
		return headers, "", true
	}
	// A fork routed to another origin may not use the client's credential. The
	// protocol headers still describe the cloned body and travel with it.
	if !isOpenAI && forkSameOrigin(endpoint, s.cfg.TargetURL) {
		return forkHeadersFor(ctx.AuthHeader, false), s.cfg.APIKey, true
	}
	return nil, "", false
}

// forkSameOrigin compares scheme and host of two upstream URLs. Empty or
// unparsable values count as a mismatch: the answer decides whether a credential
// travels, so it has to fail closed. A value without a host ("api.example.com",
// "https:api.example.com") has no origin to compare and is a mismatch too.
func forkSameOrigin(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	if ua.Host == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// forkHeadersFor assembles the headers a fork may carry: the protocol headers that
// describe the cloned body, plus the credential when it is allowed to travel. The
// body keeps beta-gated fields, so anthropic-beta has to go with it on every path;
// an upstream that receives them without the header answers 400.
func forkHeadersFor(h http.Header, withCredential bool) http.Header {
	out := http.Header{}
	if withCredential {
		if auth := h.Get("Authorization"); auth != "" {
			out.Set("Authorization", auth)
		}
		if apiKey := h.Get("X-Api-Key"); apiKey != "" {
			out.Set("X-Api-Key", apiKey)
		}
	}
	if beta := h.Get("anthropic-beta"); beta != "" {
		out.Set("anthropic-beta", beta)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// forkOriginForLog reduces an upstream URL to scheme and host, so a log line about
// a skipped fork cannot carry credentials or query secrets from the config.
func forkOriginForLog(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<invalid target>"
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// doForkCall makes the API call and parses the text response.
// Uses apiKey for x-api-key auth; falls back to origHeaders for subscription forwarding.
// When isOpenAI is true, uses /v1/chat/completions endpoint and OpenAI response format.
// Uses s.httpClient to share the main request's connection pool and DeepSeek KV-cache.
func (s *Server) doForkCall(endpoint, apiKey string, origHeaders http.Header, reqBody []byte, isOpenAI bool) (ForkResponse, error) {
	client := s.httpClient

	var apiURL string
	if isOpenAI {
		apiURL = endpoint + "/v1/chat/completions"
	} else {
		apiURL = endpoint + "/v1/messages"
	}

	httpReq, err := http.NewRequest("POST", apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return ForkResponse{}, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	if isOpenAI {
		if origHeaders != nil {
			if auth := origHeaders.Get("Authorization"); auth != "" {
				httpReq.Header.Set("Authorization", auth)
			} else if xKey := origHeaders.Get("X-Api-Key"); xKey != "" {
				httpReq.Header.Set("Authorization", "Bearer "+xKey)
			}
		}
		if apiKey != "" && httpReq.Header.Get("Authorization") == "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
	} else {
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		// The fork body keeps beta-gated fields of the original request (e.g.
		// thinking.block_binding); without the original anthropic-beta header
		// the API rejects them as extra inputs (HTTP 400).
		if origHeaders != nil {
			if beta := origHeaders.Get("anthropic-beta"); beta != "" {
				httpReq.Header.Set("anthropic-beta", beta)
			}
		}
		// Subscription clients (Claude Code OAuth) authenticate with
		// Authorization: Bearer; their token must never be moved into x-api-key.
		switch {
		case origHeaders != nil && strings.HasPrefix(origHeaders.Get("Authorization"), "Bearer "):
			httpReq.Header.Set("Authorization", origHeaders.Get("Authorization"))
		case apiKey != "":
			httpReq.Header.Set("x-api-key", apiKey)
		case origHeaders != nil:
			if xKey := origHeaders.Get("X-Api-Key"); xKey != "" {
				httpReq.Header.Set("x-api-key", xKey)
			}
		}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return ForkResponse{}, fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ForkResponse{}, fmt.Errorf("read response: %w", err)
	}

	// A rejected credential answers with a non-JSON or empty body, and without the
	// status the log line cannot tell that from a malformed response.
	var parsed ForkResponse
	var parseErr error
	if isOpenAI {
		parsed, parseErr = parseOpenAIResponse(body)
	} else {
		parsed, parseErr = parseAnthropicResponse(body)
	}
	if parseErr != nil && resp.StatusCode >= 400 {
		return ForkResponse{}, fmt.Errorf("status %d: %w", resp.StatusCode, parseErr)
	}
	return parsed, parseErr
}

func parseAnthropicResponse(body []byte) (ForkResponse, error) {
	var apiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &apiResp); err != nil {
		return ForkResponse{}, fmt.Errorf("unmarshal: %w", err)
	}

	if apiResp.Error != nil {
		return ForkResponse{}, fmt.Errorf("api error: %s: %s", apiResp.Error.Type, apiResp.Error.Message)
	}

	var text string
	for _, block := range apiResp.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}

	return ForkResponse{
		Content: text,
		Usage: ReflectionUsage{
			InputTokens:              apiResp.Usage.InputTokens,
			OutputTokens:             apiResp.Usage.OutputTokens,
			CacheReadInputTokens:     apiResp.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: apiResp.Usage.CacheCreationInputTokens,
		},
	}, nil
}

func parseOpenAIResponse(body []byte) (ForkResponse, error) {
	type chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			CacheHitTokens   int `json:"prompt_cache_hit_tokens"`
			CacheMissTokens  int `json:"prompt_cache_miss_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	var apiResp chunk

	// Try plain JSON first (stream:false responses).
	err := json.Unmarshal(body, &apiResp)
	if err != nil {
		// SSE mode: collect content from all chunks, usage from last
		bodyStr := string(body)
		var lastErr error
		for _, line := range strings.Split(bodyStr, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := line[6:]
			if payload == "[DONE]" {
				continue
			}
			var ck chunk
			if err2 := json.Unmarshal([]byte(payload), &ck); err2 != nil {
				lastErr = err2
				continue
			}
			// Accumulate delta content
			for _, choice := range ck.Choices {
				apiResp.Choices = append(apiResp.Choices, choice)
			}
			// Usage: only last chunk carries it
			if ck.Usage.PromptTokens > 0 {
				apiResp.Usage = ck.Usage
			}
			if ck.Error != nil {
				apiResp.Error = ck.Error
			}
		}
		if len(apiResp.Choices) == 0 && apiResp.Error == nil {
			if lastErr != nil {
				return ForkResponse{}, fmt.Errorf("unmarshal SSE: %w", lastErr)
			}
			return ForkResponse{}, fmt.Errorf("unmarshal: %w (no JSON)", err)
		}
	}

	if apiResp.Error != nil {
		return ForkResponse{}, fmt.Errorf("api error: %s", apiResp.Error.Message)
	}

	var text string
	for _, choice := range apiResp.Choices {
		if choice.Message.Content != "" {
			text += choice.Message.Content
		} else {
			text += choice.Delta.Content
		}
	}

	return ForkResponse{
		Content: text,
		Usage: ReflectionUsage{
			InputTokens:              apiResp.Usage.PromptTokens,
			OutputTokens:             apiResp.Usage.CompletionTokens,
			CacheReadInputTokens:     apiResp.Usage.CacheHitTokens,
			CacheCreationInputTokens: apiResp.Usage.CacheMissTokens,
		},
	}, nil
}

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/proxy"
)

// AssembleRequest is the input for the POST /api/assemble endpoint.
type AssembleRequest struct {
	SessionID   string `json:"session_id"`
	Project     string `json:"project"`
	Messages    []any  `json:"messages"`
	TokenBudget int    `json:"token_budget,omitempty"`
	KeepRecent  int    `json:"keep_recent,omitempty"`
}

// AssembleResponse is the output of the POST /api/assemble endpoint.
type AssembleResponse struct {
	Messages        []any   `json:"messages"`
	InjectedIDs     []int64 `json:"injected_ids"`
	EstimatedTokens int     `json:"estimated_tokens"`
	BriefingLength  int     `json:"briefing_length"`
}

// handleAssemble runs the full context-assembly pipeline and returns enriched messages.
func (s *Server) handleAssemble(w http.ResponseWriter, r *http.Request) {
	var req AssembleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, `{"error":"messages must not be empty"}`, http.StatusBadRequest)
		return
	}

	// Defaults
	keepRecent := req.KeepRecent
	if keepRecent <= 0 {
		keepRecent = 20
	}
	tokenBudget := req.TokenBudget
	if tokenBudget <= 0 {
		tokenBudget = 180_000
	}

	// Simple token estimator (heuristic, no tokenizer available here)
	estimateFn := func(text string) int {
		return int(float64(len(text)) / 3.6)
	}

	messages := make([]any, len(req.Messages))
	copy(messages, req.Messages)

	var allInjectedIDs []int64

	// Step 1: Generate briefing via daemon RPC
	briefingText := ""
	if s.handler != nil {
		resp := s.handler.Handle(RPCRequest{
			Method: "generate_briefing",
			Params: map[string]any{"project": req.Project, "session_id": req.SessionID},
		})
		if resp.Error == "" && len(resp.Result) > 0 {
			var br struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(resp.Result, &br) == nil && br.Text != "" {
				briefingText = br.Text
			} else {
				// Fallback: raw string
				briefingText = strings.Trim(string(resp.Result), `"`)
			}
		}
	}

	// Step 2: Strip redundant system-reminders first. The briefing is injected
	// afterwards: it used to be a role:system message, and this step drops every
	// one of those (the messages array does not accept the role), so the briefing
	// never reached an assembled request (YM-275).
	messages = proxy.StripReminders(messages)

	// Step 3: Inject the briefing. The proxy puts the briefing into the first user
	// message of a Claude Code session; the assembled array has to carry it the
	// same way, as the first thing the model reads.
	if briefingText != "" {
		messages = injectBriefingMsg(messages, briefingText)
	}

	// The collapse reads stats from `original` with an index into `modified`, so
	// both arrays have to be index-identical: the snapshot is taken after the
	// strip and the briefing merge, not before them.
	originalMessages := make([]any, len(messages))
	copy(originalMessages, messages)

	// Step 4: Compress old thinking/tool_results
	compressResult := proxy.CompressContext(messages, keepRecent, req.SessionID, estimateFn)
	messages = compressResult.Messages

	// Step 5: Budget-based cutoff
	cutoff := proxy.CalcCollapseCutoff(messages, keepRecent, tokenBudget, estimateFn)

	// Step 6: Collapse if cutoff > 0
	if cutoff > 0 {
		startTime := time.Time{} // no session start time in stateless handler
		endTime := time.Time{}
		messages = proxy.CollapseOldMessages(messages, originalMessages, cutoff, startTime, endTime, nil, nil, req.SessionID)
		// The collapse blanks messages[0], and that is where the briefing rides
		// (it must: a message of its own in front would shift the first user turn
		// the collapse protects). Re-merge it into the now blanked slot, otherwise
		// every request above the budget arrives without its memory context.
		if briefingText != "" {
			messages = remergeBriefingAfterCollapse(messages, briefingText)
		}
	}

	// Step 7: Associative context — hybrid_search on last user message
	if s.handler != nil {
		userQuery := lastUserText(messages)
		if userQuery != "" {
			searchResp := s.handler.Handle(RPCRequest{
				Method: "hybrid_search",
				Params: map[string]any{
					"query":   truncateStr(userQuery, 200),
					"limit":   float64(6),
					"project": req.Project,
				},
			})
			if searchResp.Error == "" && len(searchResp.Result) > 0 {
				injectedIDs := injectAssociativeContext(&messages, searchResp.Result, req.Project)
				allInjectedIDs = append(allInjectedIDs, injectedIDs...)
			}
		}
	}

	// Step 8: Fresh remember — pop recently saved learnings
	// NOTE: Disabled in proxy path (echo-loop: Claude sees own remember() output twice).
	// Kept here for OpenClaw/HTTP-API where remember() has no MCP tool_result feedback.
	// TODO: In OpenClaw, evaluate whether the calling environment provides tool_result
	// feedback for remember(). If yes, this becomes redundant here too.
	if s.handler != nil {
		remResp := s.handler.Handle(RPCRequest{
			Method: "pop_recent_remember",
			Params: map[string]any{},
		})
		if remResp.Error == "" && len(remResp.Result) > 0 {
			injectedIDs := injectFreshRemember(&messages, remResp.Result)
			allInjectedIDs = append(allInjectedIDs, injectedIDs...)
		}
	}

	// Step 9: Inject InlineReflectionHint (Bohrhammer principle)
	injectReflectionHint(&messages)

	// Step 10: Estimate token count
	estimatedTokens := 0
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			estimatedTokens += estimateFn(c)
		case []any:
			for _, block := range c {
				if b, ok := block.(map[string]any); ok {
					if text, ok := b["text"].(string); ok {
						estimatedTokens += estimateFn(text)
					}
				}
			}
		}
	}

	if allInjectedIDs == nil {
		allInjectedIDs = []int64{}
	}

	resp := AssembleResponse{
		Messages:        messages,
		InjectedIDs:     allInjectedIDs,
		EstimatedTokens: estimatedTokens,
		BriefingLength:  len(briefingText),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// lastUserText returns the text content of the last user message.
func lastUserText(messages []any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if m["role"] != "user" {
			continue
		}
		return extractTextFromContent(m["content"])
	}
	return ""
}

// extractTextFromContent extracts plain text from a message content field.
// Handles both string content and []any block arrays.
func extractTextFromContent(content any) string {
	switch c := content.(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var parts []string
		for _, block := range c {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if b["type"] == "text" {
				if text, ok := b["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	return ""
}

// truncateStr truncates s to at most maxLen runes.
func truncateStr(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

// injectAssociativeContext parses hybrid_search results, filters by threshold,
// injects above-threshold results into the last user message, and returns injected IDs.
func injectAssociativeContext(messages *[]any, searchResult json.RawMessage, project string) []int64 {
	const threshold = 0.020
	const maxResults = 2

	type hybridResult struct {
		ID      string  `json:"id"`
		Content string  `json:"content"`
		Snippet string  `json:"snippet"`
		Score   float64 `json:"score"`
		Project string  `json:"project"`
	}
	var wrapped struct {
		Results []hybridResult `json:"results"`
	}
	if err := json.Unmarshal(searchResult, &wrapped); err != nil {
		return nil
	}

	var lines []string
	var injectedIDs []int64

	for _, r := range wrapped.Results {
		if len(injectedIDs) >= maxResults {
			break
		}
		if r.Score < threshold {
			continue
		}
		text := r.Content
		if text == "" {
			text = r.Snippet
		}
		if text == "" {
			continue
		}
		var line string
		if r.ID != "" {
			var idVal int64
			if _, err := fmt.Sscanf(r.ID, "%d", &idVal); err == nil && idVal > 0 {
				line = fmt.Sprintf("[ID:%d] %s", idVal, truncateStr(text, 200))
				injectedIDs = append(injectedIDs, idVal)
			} else {
				line = truncateStr(text, 200)
			}
		} else {
			line = truncateStr(text, 200)
		}
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return nil
	}

	contextText := fmt.Sprintf("[yesmem associative context]\n%s\n%s\n[/yesmem context]",
		strings.Join(lines, "\n"),
		proxy.InlineReflectionHint,
	)

	*messages = appendToLastUserMessage(*messages, contextText)
	return injectedIDs
}

// injectFreshRemember parses pop_recent_remember results and injects them above the last user message.
func injectFreshRemember(messages *[]any, result json.RawMessage) []int64 {
	type recentItem struct {
		ID   int64  `json:"id"`
		Text string `json:"text"`
	}
	var resp struct {
		Items []recentItem `json:"items"`
	}
	if err := json.Unmarshal(result, &resp); err != nil || len(resp.Items) == 0 {
		return nil
	}

	var lines []string
	var injectedIDs []int64
	for _, item := range resp.Items {
		if item.ID > 0 {
			lines = append(lines, fmt.Sprintf("- [ID:%d] %s", item.ID, truncateStr(item.Text, 200)))
			injectedIDs = append(injectedIDs, item.ID)
		} else {
			lines = append(lines, "- "+truncateStr(item.Text, 200))
		}
	}

	contextText := fmt.Sprintf("[yesmem fresh memory]\nGerade gelernt:\n%s\n%s\n[/yesmem fresh memory]",
		strings.Join(lines, "\n"),
		proxy.InlineReflectionHint,
	)

	*messages = appendToLastUserMessage(*messages, contextText)
	return injectedIDs
}

// appendToLastUserMessage adds a text content block to the last user message.
// Converts string content to content blocks if needed.
func appendToLastUserMessage(messages []any, text string) []any {
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if m["role"] == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return messages
	}

	msg := messages[lastUserIdx].(map[string]any)
	var blocks []any
	switch c := msg["content"].(type) {
	case string:
		blocks = []any{
			map[string]any{"type": "text", "text": c},
		}
	case []any:
		blocks = make([]any, len(c))
		copy(blocks, c)
	default:
		return messages
	}

	blocks = append(blocks, map[string]any{
		"type": "text",
		"text": "\n" + text,
	})

	newMsg := make(map[string]any, len(msg))
	for k, v := range msg {
		newMsg[k] = v
	}
	newMsg["content"] = blocks

	result := make([]any, len(messages))
	copy(result, messages)
	result[lastUserIdx] = newMsg
	return result
}

// injectReflectionHint appends the Bohrhammer InlineReflectionHint to the last user message.
func injectReflectionHint(messages *[]any) {
	*messages = appendToLastUserMessage(*messages, proxy.InlineReflectionHint)
}

// briefingOpen and briefingClose wrap the injected memory context. The markers
// are what makes the injection idempotent: a client that feeds the assembled
// array back as its own history must not accumulate one copy per turn.
const (
	briefingOpen  = "<MANDATORY_BRIEFING>"
	briefingClose = "</MANDATORY_BRIEFING>"
)

// injectBriefingMsg merges the briefing into the FIRST USER message. It must not
// be a message of its own: the collapse keeps messages[0] as the slot of the
// first user turn and blanks its content, so a separate briefing in front of it
// would be blanked on the first collapse (YM-275). The proxy carries the briefing
// the same way for Claude Code: the first user message contains it.
func injectBriefingMsg(messages []any, briefingText string) []any {
	for i, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			return mergeBriefingAt(messages, briefingText, i)
		}
	}
	// No user message to merge into: the briefing opens the request.
	result := make([]any, 0, len(messages)+1)
	result = append(result, map[string]any{"role": "user", "content": briefingBlock(briefingText)})
	return append(result, messages...)
}

// remergeBriefingAfterCollapse puts the briefing back into the slot the collapse
// blanked. It must address messages[0] and not "the first user message": after a
// collapse that message is the freshly built archive block, and merging there
// would deliver the briefing twice (once in the slot, once in the archive).
func remergeBriefingAfterCollapse(messages []any, briefingText string) []any {
	if len(messages) == 0 {
		return messages
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if carriesBriefing(msg["content"]) {
			// The briefing rode in a message the collapse kept: nothing to do, a
			// second copy would only spend tokens.
			return messages
		}
	}
	return mergeBriefingAt(messages, briefingText, 0)
}

// mergeBriefingAt prefixes the briefing to message idx, replacing a briefing that
// is already there.
func mergeBriefingAt(messages []any, briefingText string, idx int) []any {
	if idx < 0 || idx >= len(messages) {
		return messages
	}
	msg, ok := messages[idx].(map[string]any)
	if !ok {
		return messages
	}
	block := briefingBlock(briefingText)

	out := make(map[string]any, len(msg))
	for k, v := range msg {
		out[k] = v
	}
	switch c := msg["content"].(type) {
	case string:
		out["content"] = block + stripBriefing(c)
	case []any:
		blocks := make([]any, 0, len(c)+1)
		blocks = append(blocks, map[string]any{"type": "text", "text": block})
		for _, raw := range c {
			if b, ok := raw.(map[string]any); ok && isBriefingBlock(b) {
				continue // the previous briefing is replaced, not duplicated
			}
			blocks = append(blocks, raw)
		}
		out["content"] = reorderBriefingBlock(blocks)
	default:
		// Foreign content shape: keep it and add the briefing beside it, never
		// replace what the caller sent.
		out["content"] = []any{
			map[string]any{"type": "text", "text": block},
			c,
		}
	}

	result := make([]any, len(messages))
	copy(result, messages)
	result[idx] = out
	return result
}

// briefingBlock renders the wrapper. It ends with a blank line so the briefing
// stays a block of its own in front of the message text.
func briefingBlock(briefingText string) string {
	return briefingOpen + "\n" + briefingText + "\n" + briefingClose + "\n\n"
}

// carriesBriefing reports whether content opens with a complete briefing block.
// Both markers are required and the open one has to start the content: a message
// that merely MENTIONS the marker is text the caller wrote, and it must neither
// suppress the injection (that would let a message body switch the memory
// context off) nor be deleted.
func carriesBriefing(content any) bool {
	switch c := content.(type) {
	case string:
		return strings.HasPrefix(c, briefingOpen) && strings.Contains(c, briefingClose)
	case []any:
		for _, raw := range c {
			if b, ok := raw.(map[string]any); ok && isBriefingBlock(b) {
				return true
			}
		}
	}
	return false
}

// isBriefingBlock reports whether a content block is the injected briefing: it
// has to START with the marker and carry the closing one.
func isBriefingBlock(b map[string]any) bool {
	text, _ := b["text"].(string)
	return strings.HasPrefix(text, briefingOpen) && strings.Contains(text, briefingClose)
}

// stripBriefing removes a complete briefing block from content. A marker without
// its closing tag is a quotation, not a briefing, and is left alone.
func stripBriefing(content string) string {
	for {
		start := strings.Index(content, briefingOpen)
		if start < 0 {
			return content
		}
		if start != 0 {
			// A briefing only ever starts the content it was merged into.
			return content
		}
		end := strings.Index(content[start:], briefingClose)
		if end < 0 {
			return content
		}
		content = content[start+end+len(briefingClose):]
		content = strings.TrimLeft(content, "\n")
	}
}

// reorderBriefingBlock keeps a tool_result block first. The messages array
// requires a tool_result to answer its tool_use immediately, so a text block
// prepended in front of it is rejected upstream; the briefing then follows it.
func reorderBriefingBlock(blocks []any) []any {
	if len(blocks) < 2 {
		return blocks
	}
	first, ok := blocks[1].(map[string]any)
	if !ok {
		return blocks
	}
	if first["type"] != "tool_result" {
		return blocks
	}
	reordered := make([]any, 0, len(blocks))
	reordered = append(reordered, blocks[1])
	reordered = append(reordered, blocks[0])
	reordered = append(reordered, blocks[2:]...)
	return reordered
}

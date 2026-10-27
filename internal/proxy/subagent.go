package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// isSubagentRequest is the Anthropic wire's entry into the shared verdict. The
// Anthropic request shape carries a thinking field, so the fallback at the end of
// subagentVerdict speaks here.
//
// A parent-session header is the authoritative opencode marker; the body
// heuristics in subagentVerdict cover Claude Code.
// Consequence by design: a child session runs without briefing, stub cycles,
// collapse and emergency brake, exactly like a Claude Code subagent. Because
// opencode child sessions carry their own session id, they also carry their own
// thread state — a long-running child therefore has no proxy-side context cap.
func isSubagentRequest(h http.Header, messages []any, req map[string]any) bool {
	return subagentVerdict(h, messages, req, true)
}

// subagentVerdict is the one subagent classification both wires call, so the
// same client cannot be a subagent on /v1/messages and a main session on
// /v1/chat/completions because the endpoint decided it (YM-461).
//
// wireCarriesThinking states a protocol fact, not a preference: the Anthropic
// message shape has a thinking field and a client that declares nothing else
// while omitting it is a Claude Code subagent. The OpenAI shapes have no such
// field (openai_translate writes reasoning_content as a content block inside a
// message, never as req["thinking"]), so its absence there says nothing and the
// fallback stays silent: applied verbatim it would turn every markerless
// OpenAI/Codex main session into a subagent.
func subagentVerdict(h http.Header, messages []any, req map[string]any, wireCarriesThinking bool) bool {
	if hasSubagentDeclaration(h, req) {
		return true
	}
	if hasMainSessionMarker(h, req) {
		return false
	}
	// Fallback for clients that declare nothing; the marker check already ran.
	if !wireCarriesThinking || len(messages) == 0 {
		return false
	}
	return !hasThinkingField(req)
}

// hasSubagentDeclaration reports the client's own statement that this request
// belongs to a subagent: a parent-session header, the agent-type header of a
// legacy fork build, or a body marker. Both the classification and the
// fork-budget view (isDeclaredSubagent) rest on it, so the two cannot drift.
func hasSubagentDeclaration(h http.Header, req map[string]any) bool {
	// The parent header is the authoritative opencode marker: it is only sent
	// when a parent session exists.
	if hasParentSessionHeader(h) {
		return true
	}
	// Legacy fork builds (< .173) label a child with the agent-type header and
	// do not always send a parent session; the stream attribution honours that
	// header on its own (subagentStreamInfo), so this decision must too.
	if strings.EqualFold(headerCI(h, "X-Opencode-Agent-Type"), "subagent") {
		return true
	}
	// An explicit client marker outranks every main-session marker: the
	// entrypoint is the client's own statement about what it is.
	return hasSubagentClientMarker(req)
}

// isDeclaredSubagent is the fork-budget view of the subagent question: only
// client-declared markers count, never the thinking fallback of
// isSubagentRequest. A Claude Code subagent shares the parent's session id and
// must not spend the parent's fork budget, but a client that merely omits the
// thinking field is a main session and keeps its budget.
func isDeclaredSubagent(h http.Header, req map[string]any) bool {
	return hasSubagentDeclaration(h, req)
}

// hasMainSessionMarker reports whether the client identified itself as a main
// session. A missing thinking field alone is NOT evidence of a subagent:
// clients and models without extended thinking never send one, and treating
// them as subagents switched briefing, stubs, collapse and the emergency brake
// off for the whole session.
func hasMainSessionMarker(h http.Header, req map[string]any) bool {
	// opencode sends its session id on every request (affinity header on older
	// builds); a child session also carries the parent header, which is checked
	// before this function. Reusing opencodeSessionID keeps the marker list from
	// drifting away from the thread-id resolution.
	if opencodeSessionID(h) != "" {
		return true
	}
	return strings.Contains(systemText(req), "cc_entrypoint=cli")
}

// systemText returns the text of the system prompt, whether the client sent it
// as a plain string or as the first block of the list form. Both forms occur in
// practice; reading only the list form dropped the entrypoint marker, and the
// request fell through to the thinking heuristic.
func systemText(req map[string]any) string {
	if req == nil {
		return ""
	}
	switch sys := req["system"].(type) {
	case string:
		return sys
	case []any:
		if len(sys) == 0 {
			return ""
		}
		if b, ok := sys[0].(map[string]any); ok {
			text, _ := b["text"].(string)
			return text
		}
	}
	return ""
}

// hasSubagentClientMarker reports the positive subagent markers of the body:
// the SDK entrypoint, the Haiku model (extraction pipeline, lightweight
// subagent) and Claude Code's metadata.user_id agent type. All three are
// client-declared, unlike the thinking heuristic.
func hasSubagentClientMarker(req map[string]any) bool {
	if req == nil {
		return false
	}
	if strings.Contains(systemText(req), "cc_entrypoint=sdk-ts") {
		return true
	}
	model, _ := req["model"].(string)
	if strings.Contains(model, "haiku") {
		return true
	}
	return metadataAgentType(req) == "subagent"
}

// metadataAgentType reads metadata.user_id.agent_type. Claude Code puts its
// own JSON into user_id, and an Agent-tool subagent declares itself there; the
// stream attribution (isSubagentFromBody) already trusts this marker.
func metadataAgentType(req map[string]any) string {
	meta, ok := req["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	raw, _ := meta["user_id"].(string)
	if raw == "" {
		return ""
	}
	var parsed struct {
		AgentType string `json:"agent_type"`
	}
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return ""
	}
	return parsed.AgentType
}

// hasParentSessionHeader reports whether the request carries an opencode
// parent-session marker. The header is only sent when a parent session exists,
// so its presence identifies a subagent.
func hasParentSessionHeader(h http.Header) bool {
	for _, name := range parentHeaderNames {
		if headerCI(h, name) != "" {
			return true
		}
	}
	return false
}

// hasThinkingField reports whether the request asks for extended thinking.
// Absent thinking is a weak signal: clients and models that cannot do extended
// thinking never send the field, and that must not turn them into subagents.
func hasThinkingField(req map[string]any) bool {
	if req == nil {
		return false
	}
	_, ok := req["thinking"]
	return ok
}

// injectDocsHintForSubagent adds a docs-available reminder to a subagent's messages.
// Returns the original messages unchanged if hint is empty or messages is nil.
func injectDocsHintForSubagent(messages []any, hint string) []any {
	if len(messages) == 0 || hint == "" {
		return messages
	}
	injected := make([]any, len(messages), len(messages)+1)
	copy(injected, messages)
	injected = append(injected, map[string]any{
		"role":    "user",
		"content": hint,
	})
	return injected
}

// getDocsHint fetches the cached docs hint from daemon (5min TTL on daemon side).
func (s *Server) getDocsHint(project string) string {
	params := map[string]any{}
	if project != "" {
		params["project"] = project
	}
	result, err := s.queryDaemon("get_docs_hint", params)
	if err != nil {
		return ""
	}
	var resp struct {
		DocsHint string `json:"docs_hint"`
	}
	if json.Unmarshal(result, &resp) != nil {
		return ""
	}
	return resp.DocsHint
}

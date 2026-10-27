package proxy

import "net/http"

// opencodeSessionID returns opencode's native session ID from request headers.
// opencode sets x-opencode-session for opencode-branded providers and
// x-session-affinity for all others (anthropic, openai, deepseek, ...). Since
// the 1.18.34 fork merge (builds >= 1.18.34-patched.173, Learning #96632) every
// request additionally carries x-opencode-session-id (upstream #52370), which is
// preferred over the affinity fallback. See
// packages/opencode/src/session/llm/request.ts in the opencode repo.
func opencodeSessionID(h http.Header) string {
	if sid := headerCI(h, "x-opencode-session"); sid != "" {
		return sid
	}
	if sid := headerCI(h, "x-opencode-session-id"); sid != "" {
		return sid
	}
	return headerCI(h, "x-session-affinity")
}

// anthropicThread resolves the thread ID for /v1/messages requests and says
// whether that key names a session.
// Priority: X-Claude-Code-Session-Id header > opencode session header
// ("opencode:<ses_id>", same form as the OpenAI parity path and the MCP layer)
// > metadata.user_id session (older Claude Code) > DeriveThreadID.
// Claude Code never sends the opencode headers, so its resolution is unchanged.
// Without the opencode branch, all opencode sessions hash to the same thread
// ID (identical system prompt) and share per-thread state like the briefing cache.
//
// isSession is false exactly for the last fallback: a hash of the message
// content is a cache key, not an identity, so anything that acts on behalf of
// the session (skill tracking) must not treat it as one.
//
// The session value is client input and becomes the thread key: it is printed in
// the "tid=" slot of every journal line of that thread and it names the
// per-thread caches. It is therefore derived from the token form (journalToken),
// not from the raw header: a value carrying a space or a bracket would close the
// "[req N <project> tid=...]" frame early and pass the rest off as journal
// decoration (YM-447). Real ids (ses_…, UUIDs) are tokens already, so nothing is
// lost; an id that sanitizes to nothing cannot name a thread, and the next rule
// speaks instead.
func anthropicThread(req map[string]any, h http.Header) (threadID string, isSession bool) {
	if cc := h.Get("X-Claude-Code-Session-Id"); cc != "" {
		if key := journalToken(cc); key != "" {
			return key, true
		}
	}
	if sid := opencodeSessionID(h); sid != "" {
		if key := journalToken(sid); key != "" {
			return "opencode:" + key, true
		}
	}
	if tid := extractSessionID(req, "", ""); tid != "" {
		if key := journalToken(tid); key != "" {
			return key, true
		}
	}
	return DeriveThreadID(req), false
}

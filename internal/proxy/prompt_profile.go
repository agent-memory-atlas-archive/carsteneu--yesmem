package proxy

import (
	"strings"

	"github.com/carsteneu/yesmem/internal/models"
)

// messagesRequestProfile names the prompt profile behind a /v1/messages
// request. The wire format is shared, the clients are not: opencode speaks the
// Anthropic format too, and a Claude-specific directive block must not follow
// from the wire alone.
//
// Both markers are client-declared hints, not a trust decision: a profile
// selects a set of local directive blocks and grants no access to data, and the
// fall-through is the most complete profile, so declaring nothing gains nothing.
func messagesRequestProfile(opencodeClient bool, userAgent string) models.PromptProfile {
	if opencodeClient {
		return models.ProfileOpencode
	}
	if isCodexClient(userAgent) {
		return models.ProfileCodex
	}
	return models.ProfileClaude
}

// requestProfile names the profile of an already-resolved request. The thread id
// says which client won the header precedence (anthropicThreadID puts the Claude
// Code header first and prefixes an opencode session with "opencode:"), so the
// profile cannot name a different client than the thread it is keyed under.
func requestProfile(threadID, userAgent string) models.PromptProfile {
	return messagesRequestProfile(strings.HasPrefix(threadID, "opencode:"), userAgent)
}

// isCodexClient reports whether the request comes from the Codex CLI. It sends
// no session header, so the user agent is the only client marker available.
func isCodexClient(userAgent string) bool {
	return strings.Contains(strings.ToLower(userAgent), "codex")
}

package proxy

const docsHintInterval = 10000 // tokens between docs hint re-injections

// docsCp tracks docs hint injection state per thread (independent of plan checkpoints).
var docsCp = newTokenCheckpoints()

// docsHintInject checks if a docs-available reminder should be injected.
// Works independently of plan checkpoints — ensures subagents and sessions
// without active plans still get docs reminders.
// Returns the hint text, or "" if not yet time or no docs indexed.
func (s *Server) docsHintInject(threadID string, totalTokens int, project string) string {
	if !docsCp.due(threadID, totalTokens, docsHintInterval) {
		return ""
	}

	return s.getDocsHint(project)
}

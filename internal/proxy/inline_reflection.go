package proxy

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// InlineReflectionHint is appended to every learning injection point (Bohrhammer principle).
const InlineReflectionHint = "Learnings have [ID:xxx]. At the end of every response:\n<!-- [IDs: 123, 456] --> (used learning IDs)\n<!-- [gap: topic] --> (missing domain knowledge)\n<!-- [contradiction: ID1 vs ID2: description] --> (contradiction between learnings)\nFormat ALWAYS [ID:xxx] — not 'Learning #123' or 'Learning [123]'."

var (
	// idPattern matches [ID:123] style references in thinking blocks and free text.
	idPattern = regexp.MustCompile(`\[ID:(\d+)\]`)
	// learningRefPattern matches informal "Learning #123" or "Learning [123]" references.
	// Captures any digit sequence as ID — flexible since IDs are just DB integers.
	learningRefPattern = regexp.MustCompile(`[Ll]earning\s+[#\[](\d+)\]?`)
	// idsCommentPattern matches <!-- [IDs: 123, 456] --> style HTML comment summaries.
	idsCommentPattern = regexp.MustCompile(`<!--\s*\[IDs:\s*([\d,\s]+)\]\s*-->`)
	// gapPattern matches <!-- [gap: topic text] --> knowledge-gap annotations.
	gapPattern = regexp.MustCompile(`(?s)<!--\s*\[gap:\s*(.+?)\]\s*-->`)
	// contradictionPattern matches <!-- [contradiction: description] --> annotations.
	contradictionPattern = regexp.MustCompile(`(?s)<!--\s*\[contradiction:\s*(.+?)\]\s*-->`)
)

// InlineSignals holds all signals extracted from assistant messages.
type InlineSignals struct {
	UsedIDs        []int64
	GapTopics      []string
	Contradictions []string
	// ToolErrorText is the failure text of the last user message, empty when the
	// turn carries no visible tool failure. The text, not a bool, is what the
	// fail counter needs: it is the identity of the error, and only a new error
	// may raise fail_count again (one failing build loop used to push a learning
	// to the hook's hard-block threshold on its own).
	ToolErrorText string
	// Scanned is true when an assistant response was actually found and read.
	// Callers must only treat injected learnings as noise when it is true —
	// otherwise "not referenced" just means "nothing was there to reference".
	Scanned bool
}

// ScanAssistantSignals is the exported wrapper around scanAssistantSignals.
// It is used by the httpapi package to extract inline reflection signals
// without importing the proxy package internals.
func ScanAssistantSignals(messages []any) InlineSignals {
	return scanAssistantSignals(messages)
}

// scanInlineSignals is the entry point for the automatic inline reflection.
// It is the one place where signals.enabled is consulted: with the gate closed
// nothing is scanned, so no use/noise/gap/contradiction signal can be raised.
func (s *Server) scanInlineSignals(messages []any) InlineSignals {
	if !s.cfg.InlineReflectionEnabled {
		return InlineSignals{}
	}
	return scanAssistantSignals(messages)
}

// scanAssistantSignals scans the assistant response of the previous turn in the
// message history for inline reflection signals. It looks at both thinking
// blocks and text blocks.
//
// Requests always end with the user message of the current turn, so the scan
// first skips trailing non-assistant messages and then walks back to the
// previous user message — the answer of the last turn is the only text it reads.
func scanAssistantSignals(messages []any) InlineSignals {
	// Skip the trailing user message of the current turn (and any other
	// trailing non-assistant entries) to reach the last assistant response.
	i := len(messages) - 1
	for i >= 0 {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			i--
			continue
		}
		if role, _ := msg["role"].(string); role == "assistant" {
			break
		}
		i--
	}

	// Collect text from the assistant messages of that single turn.
	var texts []string
	for ; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "user" {
			// Stop: only scan assistant messages after the last user message.
			break
		}
		if role != "assistant" {
			continue
		}
		content := msg["content"]
		switch c := content.(type) {
		case string:
			texts = append(texts, c)
		case []any:
			for _, block := range c {
				blk, ok := block.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := blk["type"].(string)
				switch blockType {
				case "thinking":
					if t, ok := blk["thinking"].(string); ok {
						texts = append(texts, t)
					}
				case "text":
					if t, ok := blk["text"].(string); ok {
						texts = append(texts, t)
					}
				}
			}
		}
	}

	if len(texts) == 0 {
		return InlineSignals{}
	}

	// Deduplicate IDs via a seen map.
	seenIDs := make(map[int64]bool)
	var usedIDs []int64

	for _, text := range texts {
		// Extract [ID:xxx] patterns (thinking blocks, free text).
		for _, m := range idPattern.FindAllStringSubmatch(text, -1) {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				if !seenIDs[id] {
					seenIDs[id] = true
					usedIDs = append(usedIDs, id)
				}
			}
		}
		// Extract informal "Learning #123" or "Learning [123]" references.
		for _, m := range learningRefPattern.FindAllStringSubmatch(text, -1) {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				if !seenIDs[id] {
					seenIDs[id] = true
					usedIDs = append(usedIDs, id)
				}
			}
		}
		// Extract <!-- [IDs: 123, 456] --> patterns (HTML comment summaries).
		for _, m := range idsCommentPattern.FindAllStringSubmatch(text, -1) {
			for _, part := range strings.Split(m[1], ",") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				if id, err := strconv.ParseInt(part, 10, 64); err == nil {
					if !seenIDs[id] {
						seenIDs[id] = true
						usedIDs = append(usedIDs, id)
					}
				}
			}
		}
	}

	// Extract gap topics.
	var gapTopics []string
	for _, text := range texts {
		for _, m := range gapPattern.FindAllStringSubmatch(text, -1) {
			topic := strings.TrimSpace(m[1])
			if topic != "" {
				gapTopics = append(gapTopics, topic)
			}
		}
	}

	// Extract contradictions.
	var contradictions []string
	for _, text := range texts {
		for _, m := range contradictionPattern.FindAllStringSubmatch(text, -1) {
			desc := strings.TrimSpace(m[1])
			if desc != "" {
				contradictions = append(contradictions, desc)
			}
		}
	}

	return InlineSignals{
		UsedIDs:        usedIDs,
		GapTopics:      gapTopics,
		Contradictions: contradictions,
		ToolErrorText:  toolErrorText(messages),
		Scanned:        true,
	}
}

// toolErrorText returns the failure text of the first failed tool_result in the
// last user message. The text doubles as the identity of the failure: the fail
// counter must not fire twice for the same error, or an error loop alone drives
// a used learning to the hook's hard-block threshold.
func toolErrorText(messages []any) string {
	// Find last user message (contains tool_results from previous assistant turn)
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] != "user" {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			break
		}
		for _, block := range blocks {
			blk, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if !toolResultIsError(blk) {
				continue
			}
			text, _ := blk["content"].(string)
			if text == "" {
				// Anthropic allows block-shaped tool result content. Serialize it
				// rather than falling back to a constant, or every such failure of
				// the thread would share one fingerprint and count only once.
				if raw, err := json.Marshal(blk["content"]); err == nil {
					text = string(raw)
				}
			}
			if text == "" {
				text = "tool_result marked is_error"
			}
			if len(text) > 200 {
				text = text[:200]
			}
			return text
		}
		break // only check the last user message
	}
	return ""
}

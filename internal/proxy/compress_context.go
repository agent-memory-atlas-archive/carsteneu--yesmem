package proxy

import (
	"fmt"
	"strings"
)

// CompressResult holds the outcome of context compression.
type CompressResult struct {
	Messages              []any
	ThinkingDropped       int
	ToolResultsCompressed int
	TokensSaved           int
}

// turnAge constants for compression thresholds.
const (
	compressMinTokens = 500 // minimum tokens to consider for compression
)

// CompressContext proactively compresses old tool_results before the
// budget-based cutoff runs. This recovers context window space from
// content that has been processed and summarized in assistant responses.
//
// Messages within the keepRecent window are never touched.
// All older messages get thinking blocks DROPPED and tool_results summarized.
//
// Thinking blocks are dropped, not rewritten, at any age outside keepRecent:
// the Anthropic API validates each thinking block's signature against its
// text, so any rewrite produces requests rejected with
// "messages.N.content.M.thinking.signature: Field required" (observed
// 2026-09-27 on opencode sessions). Claude Code strips old thinking
// client-side for the same reason. The current tool loop's thinking always
// sits inside keepRecent and is never touched.
//
// Only tool_results > 500 tokens are compressed. Messages are modified in-place.
func CompressContext(messages []any, keepRecent int, threadID string, estimateTokens TokenEstimateFunc) CompressResult {
	result := CompressResult{
		Messages: messages,
	}

	if len(messages) < 4 {
		return result
	}

	// Build tool_use_id → {name, keywords} map for summary generation
	toolInfo := buildToolUseInfoExtended(messages)

	// Calculate turn boundaries.
	// A "turn" is roughly a user+assistant message pair.
	// Count from the end to determine age.
	totalMsgs := len(messages)

	// Calculate protected tail: messages within keepRecent window stay untouched
	protectedTail := totalMsgs - keepRecent
	if protectedTail < 1 {
		protectedTail = 1 // always skip messages[0] (the original first user turn; Anthropic API has system separately)
	}

	for i := 1; i < protectedTail; i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		content := msg["content"]

		blocks, ok := content.([]any)
		if !ok {
			continue // string content — skip
		}

		modified := false
		newBlocks := make([]any, 0, len(blocks))

		for _, block := range blocks {
			b, ok := block.(map[string]any)
			if !ok {
				newBlocks = append(newBlocks, block)
				continue
			}

			blockType, _ := b["type"].(string)

			switch blockType {
			case "thinking":
				// Old thinking blocks are dropped, never rewritten: the API
				// validates each thinking block's signature against its text,
				// so ANY rewrite produces invalid requests ("thinking.signature:
				// Field required"). Dropping matches Claude Code's own history
				// stripping. No size threshold — old thinking is dead weight.
				thinking, _ := b["thinking"].(string)
				result.TokensSaved += estimateTokens(thinking)
				result.ThinkingDropped++
				modified = true

			case "tool_result":
				// tool_result content can be string or nested blocks
				resultText := extractToolResultText(b)
				tokens := estimateTokens(resultText)
				if tokens < compressMinTokens {
					newBlocks = append(newBlocks, block)
					continue
				}

				toolUseID, _ := b["tool_use_id"].(string)
				info := toolInfo[toolUseID]

				// All messages outside keepRecent: summary stub with deep_search hint
				summary := buildToolResultSummary(resultText, info, i, threadID)
				newBlock := shallowCopyMap(b)
				newBlock["content"] = summary
				newBlocks = append(newBlocks, newBlock)
				result.TokensSaved += tokens - estimateTokens(summary)
				result.ToolResultsCompressed++
				modified = true

			default:
				newBlocks = append(newBlocks, block)
			}
		}

		if modified {
			// Dropping thinking can empty a thinking-only message; empty
			// content is an invalid request. Keep the message with a minimal
			// text placeholder instead.
			if len(newBlocks) == 0 {
				newBlocks = append(newBlocks, map[string]any{
					"type": "text",
					"text": "[thinking removed]",
				})
			}
			newMsg := shallowCopyMap(msg)
			newMsg["content"] = newBlocks
			messages[i] = newMsg
			_ = role // used for potential role-specific logic later
		}
	}

	result.Messages = messages
	return result
}

// toolUseInfoExtended holds tool name and keywords for summary generation.
type toolUseInfoExtended struct {
	Name     string
	Keywords string
}

// buildToolUseInfoExtended builds tool_use_id → {name, keywords} map.
func buildToolUseInfoExtended(messages []any) map[string]toolUseInfoExtended {
	info := make(map[string]toolUseInfoExtended)
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, block := range blocks {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if b["type"] != "tool_use" {
				continue
			}
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			input, _ := b["input"].(map[string]any)
			info[id] = toolUseInfoExtended{
				Name:     name,
				Keywords: extractToolKeywords(name, input),
			}
		}
	}
	return info
}

// extractToolResultText extracts text from a tool_result's content field.
func extractToolResultText(block map[string]any) string {
	content := block["content"]
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, b := range c {
			if m, ok := b.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					sb.WriteString(text)
					sb.WriteByte('\n')
				}
			}
		}
		return sb.String()
	}
	return ""
}

// truncateMiddle keeps the first headChars and last tailChars, with a marker in between.
func truncateMiddle(text string, headChars, tailChars int) string {
	runes := []rune(text)
	if len(runes) <= headChars+tailChars+50 {
		return text // not worth truncating
	}
	head := string(runes[:headChars])
	tail := string(runes[len(runes)-tailChars:])
	dropped := len(runes) - headChars - tailChars
	return fmt.Sprintf("%s\n[...%d chars compressed...]\n%s", head, dropped, tail)
}

// truncateToolResult truncates a tool_result keeping head and tail with context.
func truncateToolResult(text string, info toolUseInfoExtended) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= 30 {
		return text // small enough to keep
	}

	var sb strings.Builder
	// Keep first 15 lines
	for i := 0; i < 15 && i < len(lines); i++ {
		sb.WriteString(lines[i])
		sb.WriteByte('\n')
	}
	sb.WriteString(fmt.Sprintf("\n[...%d lines compressed...]\n\n", len(lines)-25))
	// Keep last 10 lines
	start := len(lines) - 10
	if start < 15 {
		start = 15
	}
	for i := start; i < len(lines); i++ {
		sb.WriteString(lines[i])
		sb.WriteByte('\n')
	}
	return sb.String()
}

// buildToolResultSummary creates a summary stub for a tool_result.
func buildToolResultSummary(text string, info toolUseInfoExtended, msgIdx int, threadID string) string {
	lines := strings.Split(text, "\n")
	lineCount := len(lines)

	// Extract some structure from the content
	var hints []string

	// For code: look for func/class/type definitions
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "func ") ||
			strings.HasPrefix(trimmed, "type ") ||
			strings.HasPrefix(trimmed, "class ") ||
			strings.HasPrefix(trimmed, "def ") ||
			strings.HasPrefix(trimmed, "export ") {
			sig := truncateStr(trimmed, 60)
			hints = append(hints, sig)
			if len(hints) >= 5 {
				break
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[context compressed: %s, %d lines", info.Name, lineCount))
	if len(hints) > 0 {
		sb.WriteString(", key: ")
		sb.WriteString(strings.Join(hints, "; "))
	}
	sb.WriteString("]")
	if threadID != "" {
		sb.WriteString(fmt.Sprintf(" → get_session('%s', mode=paginated, offset=%d, limit=1)", threadID, msgIdx))
	} else if info.Keywords != "" {
		sb.WriteString(fmt.Sprintf(" → deep_search('%s')", info.Keywords))
	}
	return sb.String()
}

// shallowCopyMap creates a shallow copy of a map.
func shallowCopyMap(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type stubMatch struct {
	msgIdx     int
	stubText   string
	searchHint string
	score      int
}

// reexpandStubs checks if any stubs match the user's current query and
// replaces them with original content from search.
// Max 3 stubs, max 25% of token threshold.
//
// The replacement is what leaves the proxy: in the sawtooth path the expanded
// array is frozen right afterwards (freezeStubsAndInjectBreakpoint), so the
// restored content becomes part of the cached prefix until the prefix rotates.
// That is deliberate — rewriting a frozen prefix on every matching request
// would cost a cache write each time — but it means a re-expansion is not
// undone after the request.
func (s *Server) reexpandStubsFor(messages []any, threshold int, userQuery, project string) []any {
	if len(userQuery) < 10 {
		return messages
	}

	queryWords := significantWords(userQuery)
	var matches []stubMatch

	// Scan for stubs with deep_search hints
	for i, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}

		blocks, ok := m["content"].([]any)
		if !ok {
			// String content — check if it's a stub
			if text, ok := m["content"].(string); ok {
				if isStubText(text) {
					if hint := extractSearchHint(text); hint != "" {
						overlap := countHintOverlap(queryWords, significantWords(hint))
						if overlap >= 2 {
							matches = append(matches, stubMatch{i, text, hint, overlap})
						}
					}
				}
			}
			continue
		}

		for _, block := range blocks {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			// A compressed tool_result carries its hint in "content", a text
			// block in "text" (YM-141).
			text, _ := b["text"].(string)
			if text == "" {
				text, _ = b["content"].(string)
			}
			if !isStubText(text) {
				continue
			}
			hint := extractSearchHint(text)
			if hint == "" {
				continue
			}
			overlap := countHintOverlap(queryWords, significantWords(hint))
			if overlap >= 2 {
				matches = append(matches, stubMatch{i, text, hint, overlap})
			}
		}
	}

	if len(matches) == 0 {
		s.logger.Printf("  re-expand: no stub matches for query %q", truncateStr(userQuery, 60))
		return messages
	}

	// Sort by score descending, take top 3
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].score > matches[j].score
	})
	if len(matches) > 3 {
		matches = matches[:3]
	}
	s.logger.Printf("  re-expand: %d candidates (top: %s, overlap=%d)",
		len(matches), truncateStr(matches[0].searchHint, 40), matches[0].score)

	// Query search for each match, respecting re-expansion budget (Task #14)
	budget := NewBudget(threshold)

	for _, match := range matches {
		// Scope the archive lookup to this project: an unscoped search pulls
		// conversation content of other projects into this prompt.
		result, err := s.queryDaemon("search", map[string]any{
			"query":   match.searchHint,
			"limit":   float64(1),
			"project": project,
		})
		if err != nil {
			s.logger.Printf("  re-expand: search failed for %q: %v", truncateStr(match.searchHint, 40), err)
			continue
		}

		content := extractFirstSearchContent(result)
		if content == "" {
			s.logger.Printf("  re-expand: no content found for %q", truncateStr(match.searchHint, 40))
			continue
		}
		if len(content) <= len(match.stubText) {
			s.logger.Printf("  re-expand: content too short (%d <= %d) for %q", len(content), len(match.stubText), truncateStr(match.searchHint, 40))
			continue
		}

		// Truncate to reasonable size
		if len([]rune(content)) > 2000 {
			content = string([]rune(content)[:2000]) + "..."
		}

		contentTokens := s.countTokens(content)
		if !budget.CanSpendReExpansion(contentTokens) {
			s.logger.Printf("  re-expand: budget exceeded for %q (%d tokens)", truncateStr(match.searchHint, 40), contentTokens)
			continue
		}
		budget.SpendReExpansion(contentTokens)

		messages = replaceStubInMessage(messages, match.msgIdx, match.stubText,
			fmt.Sprintf("[re-expanded from archive]\n%s", content))

		s.logger.Printf("re-expanded stub #%d (%s) (%d tokens)",
			match.msgIdx, truncateStr(match.searchHint, 40), contentTokens)
	}

	return messages
}

// maxStubRunes bounds the text a proxy stub may occupy. The unit is the rune,
// not the byte: the producers cap their pieces in runes (60-rune signature
// hints, a 120-rune keyword hint), so a CJK stub legitimately reaches ~1300
// bytes while staying far below this bound, and a byte bound would refuse a stub
// the proxy wrote itself.
const maxStubRunes = 800

// isStubText reports whether text is one of the proxy's own stub shapes. Only
// those may be re-expanded: a real tool_result can quote a deep_search('...')
// hint (a Grep over this repo does), and replacing such a block would silently
// delete the content the model just received. A stub is a single, bounded line,
// so a message that quotes the format inside a longer explanation is not one
// (YM-275).
func isStubText(text string) bool {
	if utf8.RuneCountInString(text) > maxStubRunes || strings.Contains(text, "\n") {
		return false
	}
	return strings.HasPrefix(text, "[context compressed:") ||
		strings.HasPrefix(text, "[tool result archived") ||
		strings.HasPrefix(text, "[→]")
}

// extractSearchHint extracts the query from a deep_search('...') hint in a stub.
func extractSearchHint(stub string) string {
	idx := strings.Index(stub, "deep_search('")
	if idx < 0 {
		return ""
	}
	start := idx + len("deep_search('")
	end := strings.Index(stub[start:], "')")
	if end < 0 {
		return ""
	}
	return stub[start : start+end]
}

// countHintOverlap counts query words that occur in the hint, including a query
// word that sits inside a longer hint token. Keyword hints carry tool names and
// whole paths ("Read /repo/internal/proxy/decay.go"), so a user asking about
// "decay.go" or "proxy" has to match; plain token equality found only "read"
// (YM-141).
func countHintOverlap(query, hint map[string]bool) int {
	count := 0
	for q := range query {
		if hint[q] {
			count++
			continue
		}
		if len(q) < 4 {
			continue
		}
		for h := range hint {
			if strings.Contains(h, q) {
				count++
				break
			}
		}
	}
	return count
}

// extractFirstSearchContent gets the best content from a search result.
func extractFirstSearchContent(raw json.RawMessage) string {
	results := parseSearchResults(raw)
	if len(results) == 0 {
		return ""
	}
	return results[0]
}

// replaceStubInMessage replaces a specific stub text in a message's content blocks.
func replaceStubInMessage(messages []any, msgIdx int, oldStub, newContent string) []any {
	msg, ok := messages[msgIdx].(map[string]any)
	if !ok {
		return messages
	}

	// Handle string content
	if text, ok := msg["content"].(string); ok && text == oldStub {
		newMsg := make(map[string]any)
		for k, v := range msg {
			newMsg[k] = v
		}
		newMsg["content"] = newContent
		messages[msgIdx] = newMsg
		return messages
	}

	// Handle block content
	blocks, ok := msg["content"].([]any)
	if !ok {
		return messages
	}

	newBlocks := make([]any, len(blocks))
	replaced := false
	for i, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok {
			newBlocks[i] = block
			continue
		}
		if replaced {
			newBlocks[i] = block
			continue
		}
		// Text blocks carry the stub in "text", a compressed tool_result in
		// "content". Only the stub text is swapped: type and tool_use_id stay,
		// otherwise the API rejects the orphaned tool_result (YM-141).
		nb := shallowCopyMap(b)
		if text, _ := b["text"].(string); text == oldStub {
			nb["text"] = newContent
			newBlocks[i] = nb
			replaced = true
			continue
		}
		if content, _ := b["content"].(string); content == oldStub {
			nb["content"] = newContent
			newBlocks[i] = nb
			replaced = true
			continue
		}
		newBlocks[i] = block
	}

	newMsg := make(map[string]any)
	for k, v := range msg {
		newMsg[k] = v
	}
	newMsg["content"] = newBlocks
	messages[msgIdx] = newMsg
	return messages
}

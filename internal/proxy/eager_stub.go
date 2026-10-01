package proxy

import (
	"fmt"
	"regexp"
	"strings"
)

const eagerStubTokenThreshold = 500

// EagerStubToolResults walks the fresh tail (messages after frozenBoundary)
// and replaces large tool_result content with rule-based summaries.
// Only stubs tool_results that (a) exceed the token threshold and (b) sit in a
// finished user turn: a turn is closed by a role=user message WITHOUT any
// tool_result block (the Anthropic API requires every tool_use to be answered
// by tool_result blocks in the next user message, so a user message without
// them is a new prompt). Within the still-running turn nothing is stubbed;
// sticky-marked results (WasStubbed) stay stubbed regardless.
// Operates in the uncached zone — zero prompt cache cost.
func EagerStubToolResults(messages []any, frozenBoundary int, estimateTokens TokenEstimateFunc, opts ...EagerStubOption) []any {
	cfg := &eagerStubConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	result := make([]any, len(messages))
	copy(result, messages)

	toolIndex := eagerIndexToolUses(result)

	// Index of the last real turn boundary. tool_results may only be (newly)
	// stubbed at indices below it. Boundaries inside the frozen prefix cannot
	// enable stubbing of fresh results because i >= frozenBoundary always.
	lastBoundary := -1
	for i := len(result) - 1; i >= 0; i-- {
		msg, ok := result[i].(map[string]any)
		if !ok || msg["role"] != "user" {
			continue
		}
		if blocks, ok := msg["content"].([]any); ok {
			hasToolResult := false
			for _, block := range blocks {
				if b, ok := block.(map[string]any); ok && b["type"] == "tool_result" {
					hasToolResult = true
					break
				}
			}
			if hasToolResult {
				continue
			}
		}
		lastBoundary = i
		break
	}

	for i := frozenBoundary; i < len(result); i++ {
		msg, ok := result[i].(map[string]any)
		if !ok || msg["role"] != "user" {
			continue
		}

		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}

		turnClosed := i < lastBoundary

		hasMemoryStub := false
		if cfg.memory != nil && cfg.threadID != "" {
			for _, block := range blocks {
				b, ok := block.(map[string]any)
				if !ok || b["type"] != "tool_result" {
					continue
				}
				id, _ := b["tool_use_id"].(string)
				if id != "" && cfg.memory.WasStubbed(cfg.threadID, id) {
					hasMemoryStub = true
					break
				}
			}
		}

		if !turnClosed && !hasMemoryStub {
			continue
		}

		anyChanged := false
		newBlocks := make([]any, 0, len(blocks))
		for _, block := range blocks {
			b, ok := block.(map[string]any)
			if !ok || b["type"] != "tool_result" {
				newBlocks = append(newBlocks, block)
				continue
			}

			toolUseID, _ := b["tool_use_id"].(string)
			content := extractToolResultText(b)

			memoryHit := cfg.memory != nil && cfg.threadID != "" && toolUseID != "" &&
				cfg.memory.WasStubbed(cfg.threadID, toolUseID)

			if !memoryHit {
				if !turnClosed {
					newBlocks = append(newBlocks, block)
					continue
				}
				if estimateTokens(content) <= eagerStubTokenThreshold {
					newBlocks = append(newBlocks, block)
					continue
				}
			}

			// Label each stub by its own tool_use (parallel calls included).
			call := toolIndex[toolUseID]
			stub := buildEagerStub(call.name, call.input, content)
			newBlock := make(map[string]any)
			for k, v := range b {
				newBlock[k] = v
			}
			newBlock["content"] = stub
			newBlocks = append(newBlocks, newBlock)
			anyChanged = true

			if memoryHit {
				if cfg.stickyHits != nil {
					*cfg.stickyHits++
				}
			} else {
				if cfg.freshStubs != nil {
					*cfg.freshStubs++
				}
				if cfg.memory != nil && cfg.threadID != "" && toolUseID != "" {
					cfg.memory.RecordStubbed(cfg.threadID, toolUseID)
				}
			}
		}

		if anyChanged {
			newMsg := make(map[string]any)
			for k, v := range msg {
				newMsg[k] = v
			}
			newMsg["content"] = newBlocks
			result[i] = newMsg
		}
	}

	return result
}

type eagerToolCall struct {
	name  string
	input map[string]any
}

// eagerIndexToolUses maps every tool_use_id to its tool name and input so each
// tool_result resolves its own call — including parallel calls in one message.
func eagerIndexToolUses(messages []any) map[string]eagerToolCall {
	index := make(map[string]eagerToolCall)
	for _, msgAny := range messages {
		msg, ok := msgAny.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, block := range blocks {
			b, ok := block.(map[string]any)
			if !ok || b["type"] != "tool_use" {
				continue
			}
			id, _ := b["id"].(string)
			if id == "" {
				continue
			}
			name, _ := b["name"].(string)
			input, _ := b["input"].(map[string]any)
			index[id] = eagerToolCall{name: name, input: input}
		}
	}
	return index
}

func buildEagerStub(toolName string, input map[string]any, content string) string {
	lines := strings.Split(content, "\n")
	lineCount := len(lines)

	if toolName == "" {
		return fmt.Sprintf("[tool_result — %d lines archived]", lineCount)
	}

	switch toolName {
	case "Read":
		path, _ := input["file_path"].(string)
		funcs := eagerExtractFuncSignatures(content)
		funcStr := ""
		if len(funcs) > 0 {
			funcStr = " | " + strings.Join(funcs, ", ")
		}
		return fmt.Sprintf("[Read %s — %d lines%s]", path, lineCount, funcStr)

	case "Grep":
		pattern, _ := input["pattern"].(string)
		path, _ := input["path"].(string)
		return fmt.Sprintf("[Grep '%s' in %s — %d matches]", pattern, path, lineCount)

	case "Bash":
		cmd, _ := input["command"].(string)
		head, tail := eagerHeadTail(lines, 3, 3)
		if tail != "" {
			return fmt.Sprintf("[Bash: %s — %d lines]\n%s\n[...]\n%s", truncateStr(cmd, 80), lineCount, head, tail)
		}
		return fmt.Sprintf("[Bash: %s — %d lines]\n%s", truncateStr(cmd, 80), lineCount, head)

	case "Glob":
		pattern, _ := input["pattern"].(string)
		first := eagerFirstN(lines, 10)
		return fmt.Sprintf("[Glob '%s' — %d results]\n%s", pattern, lineCount, first)

	case "Agent":
		desc, _ := input["description"].(string)
		return fmt.Sprintf("[Agent: %s — %s]", desc, truncateStr(content, 200))

	default:
		return fmt.Sprintf("[%s result — %d lines archived]", toolName, lineCount)
	}
}

var eagerGoFuncRe = regexp.MustCompile(`(?m)^func\s+(\([^)]+\)\s+)?(\w+)\s*\(`)

func eagerExtractFuncSignatures(code string) []string {
	matches := eagerGoFuncRe.FindAllStringSubmatch(code, 20)
	var names []string
	for _, m := range matches {
		if len(m) > 2 {
			names = append(names, m[2]+"()")
		}
	}
	return names
}

func eagerHeadTail(lines []string, h, t int) (string, string) {
	if len(lines) <= h+t {
		return strings.Join(lines, "\n"), ""
	}
	return strings.Join(lines[:h], "\n"), strings.Join(lines[len(lines)-t:], "\n")
}

func eagerFirstN(lines []string, n int) string {
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n[... +%d more]", len(lines)-n)
}

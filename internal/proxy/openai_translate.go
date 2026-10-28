package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

func translateOpenAIToAnthropic(oai OpenAIChatRequest) (map[string]any, error) {
	req := map[string]any{
		"model": oai.Model,
	}
	if oai.MaxTokens > 0 {
		req["max_tokens"] = oai.MaxTokens
	}
	if oai.Temperature != nil {
		req["temperature"] = *oai.Temperature
	}
	if oai.TopP != nil {
		req["top_p"] = *oai.TopP
	}
	if oai.Stream {
		req["stream"] = true
	}
	if len(oai.Metadata) > 0 {
		req["metadata"] = oai.Metadata
	}
	if oai.ToolChoice != nil {
		req["tool_choice"] = oai.ToolChoice
	}

	var systemBlocks []any
	var nonSystemMsgs []OpenAIMessage
	for _, m := range oai.Messages {
		switch m.Role {
		case "system", "developer":
			block := map[string]any{
				"type": "text",
				"text": openAIContentText(m.Content),
			}
			if m.Role == "developer" {
				block["_openai_role"] = "developer"
			}
			systemBlocks = append(systemBlocks, block)
		default:
			nonSystemMsgs = append(nonSystemMsgs, m)
		}
	}
	if len(systemBlocks) > 0 {
		req["system"] = systemBlocks
	}

	messages, err := translateMessages(nonSystemMsgs)
	if err != nil {
		return nil, err
	}
	req["messages"] = messages

	if len(oai.Tools) > 0 {
		var tools []any
		for _, t := range oai.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": t.Function.Parameters,
			})
		}
		req["tools"] = tools
	}

	return req, nil
}

func openAIContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, block := range c {
			bm, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := bm["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	default:
		data, err := json.Marshal(content)
		if err == nil {
			return string(data)
		}
	}
	return ""
}

// openAIToolResultIsError reports whether a tool result carries a failure that
// the transport itself did not flag.
//
// opencode has no error flag on the wire: its AI-SDK conversion turns a failed
// tool result into plain text (packages/core/src/aisdk.ts, toolOutput maps
// "error" to a text output), so the only failure signal that arrives is the
// notice its shell tool appends as the last content part
// (packages/core/src/shell/result.ts notice(), joined by
// tool/plugin/shell.ts resultMessages). The notice always sits at the end, so
// only the last non-empty line is read: a command whose output merely mentions
// a failure must not count as one.
func openAIToolResultIsError(content string) bool {
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		last := strings.TrimSpace(lines[i])
		if last == "" {
			continue
		}
		switch {
		case last == "Timed out before completion":
			return true
		case strings.HasPrefix(last, "Killed by "):
			return true
		case strings.HasPrefix(last, "Exited with code "):
			return strings.TrimSpace(strings.TrimPrefix(last, "Exited with code ")) != "0"
		}
		return false
	}
	return false
}

// toolNamesByCallID maps a tool call id to the tool that produced it. The
// notice vocabulary belongs to the shell tool, so the result is only read for a
// shell; a read of a log whose last line happens to be "Exited with code 1" is
// not a failure. An unknown id stays out of the map, which errs towards no
// signal rather than a wrong one.
func toolNamesByCallID(msgs []OpenAIMessage) map[string]string {
	names := make(map[string]string)
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				names[tc.ID] = tc.Function.Name
			}
		}
		content, ok := m.Content.([]any)
		if !ok {
			continue
		}
		for _, b := range content {
			block, ok := b.(map[string]any)
			if !ok || block["type"] != "tool_use" {
				continue
			}
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			if id != "" && name != "" {
				names[id] = name
			}
		}
	}
	return names
}

// markToolResultError writes is_error only when it is set. The key is omitted
// on success on purpose: the translated request is what upstream caches, and an
// "is_error": false on every tool result would rewrite that prefix.
func markToolResultError(block map[string]any, isError bool) {
	if isError {
		block["is_error"] = true
	}
}

// toolResultIsError reports whether a tool_result block carries the failure flag.
// It is the single source for a predicate that six call sites (loop detector,
// fixation detector, assoc context, pinned reminders, intensity, inline
// reflection) used to spell out on their own.
func toolResultIsError(block map[string]any) bool {
	if block == nil || block["type"] != "tool_result" {
		return false
	}
	isErr, _ := block["is_error"].(bool)
	return isErr
}

// toolResultExitCodeFailure is the text fallback for clients that flag nothing
// but paste the shell's exit line into the tool result. It stays separate from
// toolResultIsError: the flag is a declaration, this is a heuristic over content.
func toolResultExitCodeFailure(text string) bool {
	if !strings.Contains(text, "exit code") {
		return false
	}
	return !strings.Contains(text, "exit code 0")
}

func translateMessages(msgs []OpenAIMessage) ([]any, error) {
	var result []any
	toolNames := toolNamesByCallID(msgs)

	for i := 0; i < len(msgs); i++ {
		m := msgs[i]

		switch m.Role {
		case "user":
			// Check for image_url / file blocks in content array — must be preserved
			// as structured blocks, not flattened to string by openAIContentText.
			// file blocks (PDF & Co. from opencode/AI-SDK) ride along untouched;
			// the LiteLLM gateway's docling_hook converts them to Markdown.
			if contentArr, ok := m.Content.([]any); ok {
				hasImage, hasFile := false, false
				for _, block := range contentArr {
					if bm, ok := block.(map[string]any); ok {
						if bm["type"] == "image_url" {
							hasImage = true
						}
						if bm["type"] == "file" {
							hasFile = true
						}
					}
				}
				if hasImage || hasFile {
					var blocks []any
					for _, block := range contentArr {
						bm, ok := block.(map[string]any)
						if !ok {
							continue
						}
						switch bm["type"] {
						case "text":
							if text, _ := bm["text"].(string); text != "" {
								blocks = append(blocks, map[string]any{"type": "text", "text": text})
							}
						case "image_url":
							if img := translateOpenAIImageToAnthropic(bm); img != nil {
								blocks = append(blocks, img)
							}
						case "file":
							blocks = append(blocks, map[string]any{"type": "file", "file": bm["file"]})
						}
					}
					if len(blocks) == 0 {
						blocks = append(blocks, map[string]any{"type": "text", "text": ""})
					}
					result = append(result, map[string]any{
						"role":    "user",
						"content": blocks,
					})
					continue
				}
			}

			// Check for tool_result blocks in content array (opencode format).
			if contentArr, ok := m.Content.([]any); ok {
				hasToolResult := false
				for _, block := range contentArr {
					if bm, ok := block.(map[string]any); ok && bm["type"] == "tool_result" {
						hasToolResult = true
						break
					}
				}
				if hasToolResult {
					// Don't emit text as separate user message — opencode sends the text
					// in a separate standalone user message right after.
					//
					// The tool results of one message form ONE user message with
					// tool_result blocks, exactly like the "tool" role branch below.
					// The former shape -- a message with role "tool" and a string body
					// -- is not a valid Anthropic role, and it dropped the failure flag
					// the error-driven detection depends on.
					var toolBlocks []any
					for _, block := range contentArr {
						bm, ok := block.(map[string]any)
						if !ok {
							continue
						}
						if bm["type"] == "tool_result" {
							tid, _ := bm["tool_use_id"].(string)
							content := extractToolResultContent(bm["content"])
							toolBlock := map[string]any{
								"type":        "tool_result",
								"tool_use_id": tid,
								"content":     content,
							}
							isShell := classifyLoopTool(toolNames[tid]) == loopToolShell
							markToolResultError(toolBlock, toolResultIsError(bm) || (isShell && openAIToolResultIsError(content)))
							toolBlocks = append(toolBlocks, toolBlock)
						} else if text, ok := bm["text"].(string); ok && text != "" {
							result = append(result, map[string]any{
								"role":    "user",
								"content": text,
							})
						}
					}
					if len(toolBlocks) > 0 {
						result = append(result, map[string]any{
							"role":    "user",
							"content": toolBlocks,
						})
					}
					continue // skip default text extraction
				}
			}
			result = append(result, map[string]any{
				"role":    "user",
				"content": openAIContentText(m.Content),
			})

		case "assistant":
			var blocks []any
			// Preserve reasoning_content (DeepSeek thinking mode requires it in subsequent requests)
			if m.ReasoningContent != "" {
				blocks = append(blocks, map[string]any{
					"type":     "thinking",
					"thinking": m.ReasoningContent,
				})
			}
			// Extract text from content (string or blocks)
			if text := openAIContentText(m.Content); text != "" {
				blocks = append(blocks, map[string]any{
					"type": "text",
					"text": text,
				})
			}
			// Handle tool_use blocks in content array (opencode skill format)
			if contentArr, ok := m.Content.([]any); ok {
				for _, block := range contentArr {
					if bm, ok := block.(map[string]any); ok && bm["type"] == "tool_use" {
						id, _ := bm["id"].(string)
						name, _ := bm["name"].(string)
						input := bm["input"]
						log.Printf("OPENAI-FWD: assistant tool_use in content id=%s name=%s", journalToken(id), journalToken(name))
						blocks = append(blocks, map[string]any{
							"type":  "tool_use",
							"id":    id,
							"name":  name,
							"input": input,
						})
					}
				}
			}
			// Handle standard tool_calls format
			for _, tc := range m.ToolCalls {
				var input any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					input = map[string]any{"raw": tc.Function.Arguments}
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{
					"type": "text",
					"text": "",
				})
			}
			result = append(result, map[string]any{
				"role":    "assistant",
				"content": blocks,
			})

		case "tool":
			log.Printf("OPENAI-FWD: tool msg tool_call_id=%s content_type=%T", journalToken(msgs[i].ToolCallID), msgs[i].Content)
			var toolBlocks []any
			for i < len(msgs) && msgs[i].Role == "tool" {
				content := openAIContentText(msgs[i].Content)
				block := map[string]any{
					"type":        "tool_result",
					"tool_use_id": msgs[i].ToolCallID,
					"content":     content,
				}
				isShell := classifyLoopTool(toolNames[msgs[i].ToolCallID]) == loopToolShell
				markToolResultError(block, msgs[i].IsError || (isShell && openAIToolResultIsError(content)))
				toolBlocks = append(toolBlocks, block)
				i++
			}
			i-- // back up one so the outer loop's i++ lands on the next non-tool message
			result = append(result, map[string]any{
				"role":    "user",
				"content": toolBlocks,
			})

		default:
			return nil, fmt.Errorf("unknown role: %s", m.Role)
		}
	}

	return result, nil
}

// translateOpenAIImageToAnthropic converts an OpenAI image_url content part
// to an Anthropic image block. Handles both data URIs (base64) and external URLs.
func translateOpenAIImageToAnthropic(block map[string]any) map[string]any {
	imgURL, _ := block["image_url"].(map[string]any)
	if imgURL == nil {
		return nil
	}
	url, _ := imgURL["url"].(string)
	if url == "" {
		return nil
	}
	// data:image/png;base64,<data> → source:{type:"base64", media_type, data}
	if strings.HasPrefix(url, "data:") {
		// Format: data:<media_type>;base64,<data>
		comma := strings.Index(url, ",")
		if comma < 0 {
			return nil
		}
		meta := url[5:comma] // strip "data:" prefix, stop at ","
		data := url[comma+1:]
		mediaType := strings.SplitN(meta, ";", 2)[0]
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       data,
			},
		}
	}
	// External URL → source:{type:"url", url}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url",
			"url":  url,
		},
	}
}

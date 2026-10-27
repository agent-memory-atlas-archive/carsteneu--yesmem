package proxy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	planCheckpointInterval = 10000 // tokens between plan checkpoints
	planToolScanWindow     = 4     // messages scanned for plan tool calls / plan file reads
)

var planCp = newTokenCheckpoints()

// planCheckpointInject checks if a plan checkpoint should be injected.
// Returns the checkpoint hint, or "" if not yet time or no active plan.
func (s *Server) planCheckpointInject(threadID string, totalTokens int) string {
	// Check if there's an active plan via daemon
	plan, docsHint, ok := s.getActivePlan(threadID)
	if !ok || plan == "" {
		return ""
	}

	if !planCp.due(threadID, totalTokens, planCheckpointInterval) {
		return ""
	}

	return formatPlanCheckpoint(plan, docsHint)
}

// planCheckpointReset resets the plan checkpoint counter (called after set_plan/update_plan).
func planCheckpointReset(threadID string, totalTokens int) {
	planCp.reset(threadID, totalTokens)
}

// planCheckpointClear drops the counter (called after complete_plan).
func planCheckpointClear(threadID string) {
	planCp.clear(threadID)
}

// getActivePlan fetches the active plan and docs hint from daemon. ok is false
// when the daemon did not answer at all: "no active plan" and "could not ask"
// are different facts, and a caller that injects an instruction on the first one
// must not act on the second.
func (s *Server) getActivePlan(threadID string) (plan string, docsHint string, ok bool) {
	result, err := s.queryDaemon("get_plan", map[string]any{"thread_id": threadID})
	if err != nil {
		return "", "", false
	}
	var resp struct {
		Exists   bool   `json:"exists"`
		Plan     string `json:"plan"`
		Status   string `json:"status"`
		DocsHint string `json:"docs_hint"`
	}
	if json.Unmarshal(result, &resp) != nil {
		return "", "", false
	}
	if !resp.Exists || resp.Status != "active" {
		return "", "", true
	}
	return resp.Plan, resp.DocsHint, true
}

func formatPlanCheckpoint(plan, docsHint string) string {
	checkpoint := fmt.Sprintf("[Plan Checkpoint] You have an active plan. Please update it with update_plan() — mark completed items and set current status. If everything is done, call complete_plan().\n\n[Active Plan]\n%s\n[/Active Plan]", neutralizeBlockClose(plan))
	if docsHint != "" {
		checkpoint += "\n\n" + docsHint
	}
	checkpoint += "\n[/Plan Checkpoint]"
	return checkpoint
}

// neutralizeBlockClose keeps embedded text from closing the block it sits in.
// The plan is written by the model, and a line that spells out one of the two
// closing markers would end the checkpoint early: everything after it renders as
// a top-level instruction instead of as content of the block.
//
// Only the two markers are touched, not every "[/": a plan routinely contains
// paths, links or regexes with that sequence, and rewriting those would change
// text the user expects to get back unchanged.
func neutralizeBlockClose(text string) string {
	text = strings.ReplaceAll(text, "[/Active Plan]", "[ /Active Plan]")
	return strings.ReplaceAll(text, "[/Plan Checkpoint]", "[ /Plan Checkpoint]")
}

// detectPlanFileRead scans the last 4 messages for a Read tool_use on a file
// whose path contains "plan" and ends with ".md". Returns the file path or "".
func detectPlanFileRead(messages []any) string {
	start := messageScanStart(messages)
	for i := start; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] != "tool_use" {
				continue
			}
			name, _ := block["name"].(string)
			if !strings.EqualFold(name, "read") {
				continue
			}
			input, ok := block["input"].(map[string]any)
			if !ok {
				continue
			}
			fp := toolFilePath(input)
			if fp != "" && strings.Contains(strings.ToLower(fp), "plan") && strings.HasSuffix(fp, ".md") {
				return fp
			}
		}
	}
	return ""
}

// messageScanStart returns the first index of the trailing scan window.
func messageScanStart(messages []any) int {
	start := len(messages) - planToolScanWindow
	if start < 0 {
		return 0
	}
	return start
}

// toolFilePath reads a tool call's file argument under the key each client
// uses: Claude Code sends file_path (and notebook_path for NotebookEdit), some
// OpenAI transports filePath, opencode sends path.
func toolFilePath(input map[string]any) string {
	for _, key := range []string{"file_path", "filePath", "path", "notebook_path"} {
		if v, _ := input[key].(string); v != "" {
			return v
		}
	}
	return ""
}

// isControlRune reports whether r breaks out of the single injected line it is
// embedded in: C0 controls, DEL, C1 controls, and the Unicode line and
// paragraph separators, which render as a line break without being ASCII.
func isControlRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029
}

// planNudgeName reduces a detected plan file to a single-line display name. The
// path comes from a tool input, so it can carry newlines or control characters
// that would break out of the nudge sentence it is embedded in. The result is
// bounded like the loop-warning path label: the name is neither carried into a
// persisted meta entry unbounded nor rendered into a multi-KB instruction.
func planNudgeName(path string) string {
	cleaned := strings.Map(func(r rune) rune {
		if isControlRune(r) {
			return -1
		}
		return r
	}, filepath.Base(path))
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return "plan file"
	}
	if runes := []rune(cleaned); len(runes) > maxLoopPathRunes {
		return string(runes[:maxLoopPathRunes]) + "…"
	}
	return cleaned
}

// planNudgeText is the instruction injected when a plan file is read without an
// active plan. English like every other proxy injection in this pipeline.
// name must come from planNudgeName: it is caller-supplied tool input that ends
// up inside an instruction, and only planNudgeName strips control characters.
func planNudgeText(name string) string {
	return fmt.Sprintf("You just read an implementation plan (%s). Activate it now via set_plan() so plan checkpoints work and your progress is tracked.", name)
}

// planToolBaseName strips the transport-specific MCP qualification so the same
// tool is recognized on every backend. Qualified forms seen in the wild:
// mcp__yesmem__set_plan (Claude Code) and yesmem_set_plan (opencode,
// plugins/opencode-yesmem-v2/context.ts). The other prefixes are accepted
// defensively; a bare name passes through unchanged.
func planToolBaseName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"mcp__yesmem__", "mcp_yesmem_", "yesmem_", "yesmem."} {
		if strings.HasPrefix(n, prefix) {
			return strings.TrimPrefix(n, prefix)
		}
	}
	return n
}

// planToolNamesInMessage collects tool names from both wire shapes: Anthropic
// content blocks and the OpenAI tool_calls array that opencode uses on a
// non-Anthropic provider and codex uses always.
func planToolNamesInMessage(m map[string]any) []string {
	var names []string

	if blocks, ok := m["content"].([]any); ok {
		for _, block := range blocks {
			b, ok := block.(map[string]any)
			if !ok || b["type"] != "tool_use" {
				continue
			}
			if name, _ := b["name"].(string); name != "" {
				names = append(names, name)
			}
		}
	}

	if calls, ok := m["tool_calls"].([]any); ok {
		for _, call := range calls {
			c, ok := call.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := c["function"].(map[string]any)
			if !ok {
				continue
			}
			if name, _ := fn["name"].(string); name != "" {
				names = append(names, name)
			}
		}
	}

	return names
}

// shouldNudgePlan returns true if we should inject a system-reminder nudging
// Claude to call set_plan(). Conditions: a plan file was read AND no plan is active.
func shouldNudgePlan(planFile, activePlan string) bool {
	return planFile != "" && activePlan == ""
}

// detectPlanToolCall checks if the current request contains a set_plan or
// update_plan tool call (any backend spelling, both wire formats). If so, resets
// the checkpoint counter. Called during request processing.
func (s *Server) detectPlanToolCall(messages []any, threadID string, totalTokens int) {
	if len(messages) == 0 {
		return
	}
	start := messageScanStart(messages)
	for _, msg := range messages[start:] {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "assistant" {
			continue
		}
		for _, name := range planToolNamesInMessage(m) {
			switch planToolBaseName(name) {
			case "set_plan", "update_plan":
				planCheckpointReset(threadID, totalTokens)
				return
			case "complete_plan":
				planCheckpointClear(threadID)
				return
			}
		}
	}
}

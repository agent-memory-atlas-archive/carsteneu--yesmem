package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
)

type ReplPatternSuggestion struct {
	ID              int64  `json:"id"`
	Project         string `json:"project"`
	ShapeHash       string `json:"shape_hash"`
	FirstCmdExample string `json:"first_cmd_example"`
	Count           int    `json:"count"`
	DismissCount    int    `json:"dismiss_count"`
	MatchedCap      string `json:"matched_cap"`
}

type daemonQueryFunc func(method string, params map[string]any) (json.RawMessage, error)

// reShCallInJS captures the first-arg string literal of any sh() call inside a
// REPL JS code field. Three quote styles: backtick, single, double. No escape
// unrolling — the normalizer is tolerant to residual escape sequences.
var reShCallInJS = regexp.MustCompile("(?s)\\bsh\\(\\s*(`[^`]*`|'[^']*'|\"[^\"]*\")")

// reCapTableRef captures the cap-name from a `cap_<name>__<table>` reference
// in arbitrary text. Used to associate a recorded REPL pattern with the
// already-active cap that covers it, so the suggestion path can propose
// concrete cap_search/cap_collect calls instead of generic hints.
var reCapTableRef = regexp.MustCompile(`cap_([a-z][a-z0-9_]*?)__[a-z][a-z0-9_]*`)

// extractMatchedCap returns the first cap name referenced via the
// cap_<name>__<table> naming convention in cmd, or "" if none.
func extractMatchedCap(cmd string) string {
	m := reCapTableRef.FindStringSubmatch(cmd)
	if m == nil {
		return ""
	}
	return m[1]
}

// extractShellCommandsFromMessages returns the sh() command literals of the
// newest assistant message. Only that turn is read: each turn is already seen
// by the request that follows it, so re-reading the whole history recorded the
// same command again on every later request and fired one synchronous daemon
// RPC per historical command (YM-147).
func extractShellCommandsFromMessages(messages []any) []string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		return shCommandsInMessage(msg)
	}
	return nil
}

func shCommandsInMessage(msg map[string]any) []string {
	content, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] != "tool_use" {
			continue
		}
		if block["name"] != "REPL" {
			continue
		}
		input, ok := block["input"].(map[string]any)
		if !ok {
			continue
		}
		code, ok := input["code"].(string)
		if !ok {
			continue
		}
		for _, match := range reShCallInJS.FindAllStringSubmatch(code, -1) {
			if len(match) < 2 || len(match[1]) < 2 {
				continue
			}
			out = append(out, match[1][1:len(match[1])-1])
		}
	}
	return out
}

// hasRecentREPLActivity reports whether any of the last `window` messages
// contain an assistant-role REPL tool_use block. When there has been no
// REPL activity in the recent window, the whole detection stage can
// early-exit before touching the daemon (Phase 5 activity gate: keeps
// pure text sessions and non-REPL work free of pattern-detection overhead).
func hasRecentREPLActivity(messages []any, window int) bool {
	start := 0
	if len(messages) > window {
		start = len(messages) - window
	}
	for i := start; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] != "assistant" {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, c := range content {
			block, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "tool_use" && block["name"] == "REPL" {
				return true
			}
		}
	}
	return false
}

func detectReplPatternSuggestion(messages []any, project string, call daemonQueryFunc) *ReplPatternSuggestion {
	if project == "" || call == nil {
		return nil
	}
	if !hasRecentREPLActivity(messages, 20) {
		return nil
	}
	cmds := extractShellCommandsFromMessages(messages)
	if len(cmds) == 0 {
		return nil
	}
	recorded := 0
	for _, cmd := range cmds {
		if isTrivialShape(NormalizeShellCommand(cmd)) {
			continue
		}
		hash := ShellCommandHash(cmd)
		_, _ = call("record_repl_pattern", map[string]any{
			"project":     project,
			"shape_hash":  hash,
			"example":     cmd,
			"matched_cap": extractMatchedCap(cmd),
		})
		recorded++
	}
	if recorded == 0 {
		return nil
	}
	// Suggestion-Fetch+Inject lebt in caps_inject.go:injectReplPatternSuggestionIfReady,
	// damit detect() weiterhin record-only nil-Return ist (len(m.calls)==1 in detect-Tests).
	return nil
}

func injectReminderIntoLastUserMessage(req map[string]any, body string) bool {
	msgsRaw, ok := req["messages"]
	if !ok {
		return false
	}
	msgs, ok := msgsRaw.([]any)
	if !ok {
		return false
	}
	lastIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "user" {
			lastIdx = i
			break
		}
	}
	if lastIdx == -1 {
		return false
	}
	last := msgs[lastIdx].(map[string]any)
	reminder := "<system-reminder>\n" + body + "\n</system-reminder>"
	switch c := last["content"].(type) {
	case string:
		last["content"] = c + "\n\n" + reminder
	case []any:
		last["content"] = append(c, map[string]any{
			"type": "text",
			"text": "\n\n" + reminder,
		})
	default:
		return false
	}
	return true
}

func formatReplPatternSuggestion(s ReplPatternSuggestion, project string) string {
	cap := s.MatchedCap
	return fmt.Sprintf("REPL-Pattern wiederholt sich (%d Mal) — moeglicherweise vereinfacht ein bestehender Cap das.\n\nBeispiel: %s\nShape-Hash: %s\nWahrscheinlicher Cap: %s\n\nPruefen: cap_search(name=\"%s\")\nFalls irrelevant: dismiss_repl_pattern(project=\"%s\", shape_hash=\"%s\")",
		s.Count, s.FirstCmdExample, s.ShapeHash, cap, cap, project, s.ShapeHash)
}

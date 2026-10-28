package proxy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
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
	active, ok := s.fetchActivePlan(threadID)
	if !ok || (active.Content == "" && len(active.Items) == 0) {
		return ""
	}

	if !planCp.due(threadID, totalTokens, planCheckpointInterval) {
		return ""
	}

	// A structured plan needs one line in the ledger, not its full text: the
	// text stays reachable through get_plan, which the summary points at.
	if len(active.Items) > 0 {
		return formatPlanCheckpointSummary(active.Items, active.DocsHint)
	}
	return formatPlanCheckpoint(active.Content, active.DocsHint)
}

// planCheckpointReset resets the plan checkpoint counter (called after set_plan/update_plan).
func planCheckpointReset(threadID string, totalTokens int) {
	planCp.reset(threadID, totalTokens)
}

// planCheckpointClear drops the counter (called after complete_plan).
func planCheckpointClear(threadID string) {
	planCp.clear(threadID)
}

// activePlan is the daemon's answer about one thread's plan.
type activePlan struct {
	Content  string
	Items    []StatusPlanItem
	DocsHint string
	Active   bool
	// Published reports whether the outline reached the status writer. The writer
	// only writes into a thread it has already seen, so the first request of a
	// session drops the snapshot; a caller that caches this answer must retry.
	Published bool
}

// fetchActivePlan asks the daemon for the plan and publishes its outline for the
// TUI sidebar: the per-thread status file is the only channel the TUI has, and
// a plan change has to become visible without waiting for a new session.
// ok is false only when the daemon did not answer at all.
func (s *Server) fetchActivePlan(threadID string) (activePlan, bool) {
	result, err := s.queryDaemon("get_plan", map[string]any{"thread_id": threadID})
	if err != nil {
		return activePlan{}, false
	}
	var resp struct {
		Exists   bool             `json:"exists"`
		Plan     string           `json:"plan"`
		Status   string           `json:"status"`
		DocsHint string           `json:"docs_hint"`
		Items    []StatusPlanItem `json:"items"`
	}
	if json.Unmarshal(result, &resp) != nil {
		return activePlan{}, false
	}
	if !resp.Exists || resp.Status != "active" {
		return activePlan{Published: s.publishPlanSnapshot(threadID, nil)}, true
	}
	return activePlan{
		Content:   resp.Plan,
		Items:     resp.Items,
		DocsHint:  resp.DocsHint,
		Active:    true,
		Published: s.publishPlanSnapshot(threadID, resp.Items),
	}, true
}

// publishPlanSnapshot hands the outline to the status writer. A nil outline
// clears a stale one, which is what a plan without items and a completed plan
// both need.
func (s *Server) publishPlanSnapshot(threadID string, items []StatusPlanItem) bool {
	if s.cacheStatusWriter == nil {
		// No sidebar anywhere: nothing can be lost by caching the answer.
		return true
	}
	if len(items) == 0 {
		s.cacheStatusWriter.SetPlan(threadID, nil)
		return true
	}
	snapshot := make([]StatusPlanItem, len(items))
	copy(snapshot, items)
	return s.cacheStatusWriter.SetPlan(threadID, &StatusPlan{Items: snapshot})
}

// getActivePlan fetches the active plan and docs hint from daemon. ok is false
// when the daemon did not answer at all: "no active plan" and "could not ask"
// are different facts, and a caller that injects an instruction on the first one
// must not act on the second.
func (s *Server) getActivePlan(threadID string) (plan string, docsHint string, ok bool) {
	active, ok := s.fetchActivePlan(threadID)
	if !ok {
		return "", "", false
	}
	return active.Content, active.DocsHint, true
}

func formatPlanCheckpoint(plan, docsHint string) string {
	return planCheckpointBlock(neutralizeBlockClose(plan), docsHint)
}

// formatPlanCheckpointSummary renders the compact form for a structured plan.
// The one-liner carries what a resuming agent needs first: how far the plan got,
// which step is next, and which steps are still open. Everything else stays
// reachable through get_plan.
func formatPlanCheckpointSummary(items []StatusPlanItem, docsHint string) string {
	body := neutralizeBlockClose(planSummaryLine(items))
	body += fmt.Sprintf("\n(%d items, full text via get_plan())", len(items))
	return planCheckpointBlock(body, docsHint)
}

func planCheckpointBlock(body, docsHint string) string {
	checkpoint := fmt.Sprintf("[Plan Checkpoint] You have an active plan. Please update it with update_plan() — mark completed items and set current status. If everything is done, call complete_plan().\n\n[Active Plan]\n%s\n[/Active Plan]", body)
	if docsHint != "" {
		checkpoint += "\n\n" + docsHint
	}
	checkpoint += "\n[/Plan Checkpoint]"
	return checkpoint
}

const (
	planSummaryItemRunes = 60
	planSummaryOpenMax   = 2
	// planSyncedMax bounds the set of threads whose outline was already
	// published; the thread id is client-chosen, so the set must not grow with
	// the request count.
	planSyncedMax = 1024
)

// planSummaryLine renders the ledger one-liner: progress, the next step, and up
// to two further open steps. A cancelled step counts as neither done nor open —
// and not in the denominator either, or "3/5 done" would read as two steps left
// when one of them was dropped. An unknown status counts as open, because that
// is what it looks like.
func planSummaryLine(items []StatusPlanItem) string {
	if len(items) == 0 {
		return ""
	}
	done := 0
	active := 0
	open := make([]string, 0, len(items))
	for _, it := range items {
		switch it.Status {
		case "cancelled":
			continue
		case "completed":
			done++
		default:
			open = append(open, planOneLine(it.Content))
		}
		active++
	}
	if active == 0 {
		return ""
	}
	line := fmt.Sprintf("%d/%d done", done, active)
	if len(open) == 0 {
		return line
	}
	line += ", next: " + open[0]
	rest := open[1:]
	if len(rest) > planSummaryOpenMax {
		rest = rest[:planSummaryOpenMax]
	}
	if len(rest) > 0 {
		line += ", open: " + strings.Join(rest, ", ")
	}
	return line
}

// planOneLine flattens model-written item text: a line break would break the
// single line the summary is, a control character would travel into every
// renderer of the plan, and the length bound keeps one verbose step from
// dominating the checkpoint.
func planOneLine(text string) string {
	// A control rune becomes a space rather than nothing, so "a\x00b" does not
	// turn into the single word "ab".
	stripped := strings.Map(func(r rune) rune {
		if isControlRune(r) {
			return ' '
		}
		return r
	}, text)
	flat := strings.Join(strings.Fields(stripped), " ")
	runes := []rune(flat)
	if len(runes) <= planSummaryItemRunes {
		return flat
	}
	return string(runes[:planSummaryItemRunes]) + "…"
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
// mcp__yesmem__set_plan (Claude Code), yesmem_set_plan (opencode,
// plugins/opencode-yesmem-v2/context.ts) and tools.yesmem.set_plan (opencode
// Code Mode, where the call sits inside an execute tool). The other prefixes are
// accepted defensively; a bare name passes through unchanged.
func planToolBaseName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"mcp__yesmem__", "mcp_yesmem_", "tools.yesmem.", "yesmem_", "yesmem."} {
		if strings.HasPrefix(n, prefix) {
			return strings.TrimPrefix(n, prefix)
		}
	}
	return n
}

// planCallInCode matches a plan tool call inside Code Mode, where the assistant
// message carries one tool_use named "execute" and the call sits in its code.
var planCallInCode = regexp.MustCompile(`\b(set_plan|update_plan|complete_plan)\s*\(`)

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
			name, _ := b["name"].(string)
			if name == "" {
				continue
			}
			names = append(names, name)
			// Code Mode hides the plan call inside the executor's code: the wire
			// carries one tool_use named "execute", so the name alone says nothing.
			if planToolBaseName(name) != "execute" {
				continue
			}
			input, ok := b["input"].(map[string]any)
			if !ok {
				continue
			}
			code, _ := input["code"].(string)
			for _, match := range planCallInCode.FindAllStringSubmatch(code, -1) {
				names = append(names, match[1])
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
// the checkpoint counter. Called during request processing. The return value
// reports whether a plan tool call was seen: the sidebar snapshot follows those
// calls, so it stays current even when the checkpoint gate is off.
func (s *Server) detectPlanToolCall(messages []any, threadID string, totalTokens int) bool {
	if len(messages) == 0 {
		return false
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
				return true
			case "complete_plan":
				planCheckpointClear(threadID)
				return true
			}
		}
	}
	return false
}

// syncPlanSnapshot keeps the sidebar's plan outline current. It runs outside the
// plan_checkpoint gate on purpose: the gate decides whether the ledger injects a
// reminder, while the TUI reads the outline from the status file, and a model
// with the gate off would otherwise never see its plan on screen.
//
// The fetch is a daemon round trip, so it runs once per thread per process and
// again after a plan tool call.
func (s *Server) syncPlanSnapshot(threadID string, planCallSeen bool) {
	if s.cacheStatusWriter == nil || threadID == "" {
		return
	}
	if !planCallSeen {
		s.planSyncedMu.Lock()
		synced := s.planSynced[threadID]
		s.planSyncedMu.Unlock()
		if synced {
			return
		}
	}

	// The fetch publishes the outline as a side effect. It runs before the thread
	// is marked, and only a landed snapshot counts: a daemon that is restarting (a
	// deploy does exactly that) answers nothing, and the status writer drops the
	// first publish of a session because it has not seen that thread yet. Marking
	// either case would hide the plan for the rest of the session.
	active, ok := s.fetchActivePlan(threadID)
	if !ok || !active.Published {
		return
	}

	s.planSyncedMu.Lock()
	if s.planSynced == nil {
		s.planSynced = make(map[string]bool)
	}
	// The thread id comes from the client, so a fresh id per request would grow
	// this map forever. Dropping the whole set is cheap: the worst case is one
	// extra fetch per known thread, and every entry is rebuilt on its next
	// request.
	if len(s.planSynced) > planSyncedMax {
		s.planSynced = make(map[string]bool)
	}
	s.planSynced[threadID] = true
	s.planSyncedMu.Unlock()
}

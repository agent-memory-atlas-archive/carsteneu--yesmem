package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/models"
)

// ScriptInjection carries one script entry inside a cap — Cap-Spec v1.1
// allows multiple scripts per cap (bundle caps), each with its own kind,
// runtime and schema. Only kind="tool" scripts are callable through
// execute_cap, so the catalog names those.
type ScriptInjection struct {
	Name    string
	Kind    string
	Runtime string
	Body    string
	Schema  string
}

// CapInjection carries the fields the proxy needs to render the catalog row of
// one active cap. Cap-level metadata (Name/Description/Tags) plus a slice of
// Scripts; the registerTool(...) snippets are the daemon's business
// (handler_caps.go), the proxy only names what execute_cap can call.
type CapInjection struct {
	Name        string
	Description string
	Tags        []string
	Project     string
	Scripts     []ScriptInjection
}

// decodeCapsResponse unmarshals the JSON list returned by the daemon's
// get_active_caps handler into []CapInjection. The daemon
// returns capResult structs with a nested CapabilityMeta — we only
// need the subset of Meta fields required to render registerTool() snippets.
func decodeCapsResponse(raw json.RawMessage) ([]CapInjection, error) {
	var results []struct {
		ID      int64  `json:"id"`
		Project string `json:"project,omitempty"`
		Source  string `json:"source"`
		Meta    struct {
			Name        string   `json:"cap_name"`
			Description string   `json:"cap_description"`
			Tags        []string `json:"cap_tags"`
			Scripts     []struct {
				Name    string `json:"name"`
				Kind    string `json:"kind"`
				Runtime string `json:"runtime"`
				Body    string `json:"body"`
				Schema  string `json:"schema"`
			} `json:"cap_scripts"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("decode caps response: %w", err)
	}
	caps := make([]CapInjection, 0, len(results))
	for _, r := range results {
		scripts := make([]ScriptInjection, 0, len(r.Meta.Scripts))
		for _, sc := range r.Meta.Scripts {
			scripts = append(scripts, ScriptInjection{
				Name:    sc.Name,
				Kind:    sc.Kind,
				Runtime: sc.Runtime,
				Body:    sc.Body,
				Schema:  sc.Schema,
			})
		}
		caps = append(caps, CapInjection{
			Name:        r.Meta.Name,
			Description: r.Meta.Description,
			Tags:        r.Meta.Tags,
			Project:     r.Project,
			Scripts:     scripts,
		})
	}
	return caps, nil
}

// renderCapabilitiesCatalog builds the <system-reminder>-wrapped catalog listing
// available caps without embedding full handler code. Each cap is listed with
// name and description; a small activate_cap bootstrapper is included so Claude
// can load the full handler on demand via MCP. The <caps-active> registerTool
// block it used to render is gone: the daemon generates those snippets
// (activate_cap) and the proxy copy was never called.
func renderCapabilitiesCatalog(caps []CapInjection) string {
	if len(caps) == 0 {
		return ""
	}
	sorted := make([]CapInjection, len(caps))
	copy(sorted, caps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	b.WriteString("<system-reminder>\n<caps-available>\n")
	b.WriteString("# Available Capabilities\n\n")
	b.WriteString("Activate before use: `await mcp__yesmem__activate_cap({name: \"<name>\"})` in REPL — fetches full handler from DB and returns `{code, description}`. Then `eval(result.code)` to register the tool.\n\n")
	b.WriteString("When a capability handler errors: diagnose the root cause, fix the handler, save the corrected version via `save_cap` (auto-supersedes), then retry.\n\n")

	b.WriteString("| Capability | Description |\n|---|---|\n")
	for _, cap := range sorted {
		b.WriteString(fmt.Sprintf("| %s | %s |\n", capTableCell(cap.Name), capTableCell(cap.Description)))
	}

	b.WriteString("</caps-available>\n</system-reminder>")
	return b.String()
}

// injectCapabilitiesTurnImpl is the testable core of the proxy stage. It
// queries the daemon for active caps on the given thread and, if any
// are active, inserts a user/assistant pair into the message stream. The user
// message carries the <system-reminder>-wrapped <caps-active> block;
// the assistant message is a short ack that preserves the alternating-role
// invariant the Anthropic API requires.
//
// Position choice — scans forward from msgs[0] for consecutive
// framework header pairs (user message wrapped in <system-reminder> +
// assistant "Understood..." ack) and lands the caps-pair immediately
// after the last such pair. Claude Code's per-session header is always
// the briefing + code-map in that shape, so this puts caps at a
// deterministic index (e.g. 4 after a two-pair header, 2 after a
// one-pair header, 0 if no header). Stable position across turns is
// required for Anthropic prefix-cache hit rate: caps-pair sits on the
// byte-stable header prefix, and only content below the injection
// invalidates when the active-cap set changes.
//
// Adjacency — inserting before assistant(tool_use) is safe because the
// matched assistant role in header detection requires an "Understood..."
// text ack, which never carries tool_use. If a tool_use chain follows
// the header, the caps-pair lands before it and the tool_use/tool_result
// adjacency is preserved by inserting both caps messages together.
//
// No cross-message idempotency — each proxy request comes fresh from
// Claude Code, which never resends proxy-injected system-reminders from
// prior turns, so stacking cannot occur. Removed 2026-04-17 after root
// cause analysis: the old marker-scan over-matched archive-stub summaries
// that happened to mention "<caps-active>" in plain text (e.g.,
// gotcha notes referencing this injector), silently suppressing every
// subsequent injection once such a stub entered session history.
//
// Dialect — the block body follows the client profile, not the wire: opencode
// speaks /v1/messages too and its Code Mode has no eval, so it gets the
// execute_cap catalog instead of the activate_cap bootstrap.
func injectCapabilitiesTurnImpl(
	req map[string]any,
	threadID string,
	parentThreadID string,
	profile models.PromptProfile,
	queryFn func(method string, params map[string]any) (json.RawMessage, error),
	capsCache *CapsCache,
	logger *log.Logger,
) bool {
	qp := map[string]any{"thread_id": threadID}
	if parentThreadID != "" {
		qp["parent_thread_id"] = parentThreadID
	}
	// Project scope: the daemon filters out caps scoped to other projects.
	// Derived from the request's working directory so the catalog stays
	// project-specific even when the proxy has no explicit project arg.
	if project := daemonProject(extractProjectName(req), extractWorkingDirectory(req)); project != "" {
		qp["project"] = project
	}
	raw, err := queryFn("get_active_caps", qp)
	var caps []CapInjection
	var decodeErr error
	if err == nil && len(raw) > 0 {
		caps, decodeErr = decodeCapsResponse(raw)
	}
	if len(caps) == 0 {
		if capsCache != nil {
			if cached, ok := capsCache.Get(threadID); ok {
				if fallback, ferr := decodeCapsResponse(cached); ferr == nil && len(fallback) > 0 {
					if logger != nil {
						logger.Printf("[caps-inject] fallback to cache for tid=%s: daemon err=%v decode err=%v cached=%d caps", journalToken(threadID), err, decodeErr, len(fallback))
					}
					caps = fallback
				}
			}
		}
		if len(caps) == 0 {
			if logger != nil && (err != nil || decodeErr != nil) {
				logger.Printf("[caps-inject] skip for tid=%s: daemon err=%v decode err=%v cache empty", journalToken(threadID), err, decodeErr)
			}
			return false
		}
	} else if capsCache != nil {
		capsCache.Set(threadID, raw)
	}

	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		return false
	}

	lastIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m != nil && m["role"] == "user" {
			lastIdx = i
			break
		}
	}
	if lastIdx < 0 {
		return false
	}

	insertAt := 0
	for insertAt+1 < lastIdx {
		u, uok := msgs[insertAt].(map[string]any)
		a, aok := msgs[insertAt+1].(map[string]any)
		if !uok || !aok {
			break
		}
		if u["role"] != "user" || a["role"] != "assistant" {
			break
		}
		uc, _ := u["content"].(string)
		if !strings.Contains(uc, "<system-reminder>") {
			break
		}
		ac, _ := a["content"].(string)
		if !strings.HasPrefix(ac, "Understood") {
			break
		}
		insertAt += 2
	}

	block := renderCapabilitiesCatalog(caps)
	if profile != models.ProfileClaude {
		block = renderOpencodeCapabilitiesCatalog(caps)
	}
	capsUser := map[string]any{"role": "user", "content": block}
	capsAssistant := map[string]any{"role": "assistant", "content": "Understood. I've read the active caps."}

	newMsgs := make([]any, 0, len(msgs)+2)
	newMsgs = append(newMsgs, msgs[:insertAt]...)
	newMsgs = append(newMsgs, capsUser, capsAssistant)
	newMsgs = append(newMsgs, msgs[insertAt:]...)
	req["messages"] = newMsgs

	// The REPL-pattern hint is a Claude Code construct: it names a cap function the
	// REPL can call directly plus the MCP tools around it. opencode has no REPL and
	// no bare cap function (it reaches a cap through execute_cap with fn), and the
	// recorded shapes come from Claude's bash/REPL pipelines, so the hint only
	// sends the other profiles after something that does not exist for them.
	if profile == models.ProfileClaude {
		injectReplPatternSuggestionIfReady(req, caps, queryFn)
	}

	return true
}

func injectReplPatternSuggestionIfReady(
	req map[string]any,
	caps []CapInjection,
	queryFn func(method string, params map[string]any) (json.RawMessage, error),
) {
	if queryFn == nil || len(caps) == 0 {
		return
	}
	project := ""
	for _, c := range caps {
		if c.Project != "" {
			project = c.Project
			break
		}
	}
	// All-globals threads (every cap is user-scoped, Project=="") have no
	// project anchor in the caps response — get_repl_pattern_suggestion needs
	// one to scope recorded patterns. Skip silently; resolving via thread→project
	// would require a daemon-side RPC which is out of scope for this fix.
	if project == "" {
		return
	}
	activeCapNames := make([]any, 0, len(caps))
	for _, c := range caps {
		if c.Name == "" {
			continue
		}
		// Spec § scope: user-scoped caps (Project=="") are usable in any project
		// context, so they belong in active_caps alongside the chosen project's caps.
		if c.Project == project || c.Project == "" {
			activeCapNames = append(activeCapNames, c.Name)
		}
	}
	if len(activeCapNames) == 0 {
		return
	}
	raw, err := queryFn("get_repl_pattern_suggestion", map[string]any{
		"project":     project,
		"active_caps": activeCapNames,
	})
	if err != nil || raw == nil || string(raw) == "null" {
		return
	}
	var resp struct {
		Pattern *ReplPatternSuggestion `json:"pattern"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Pattern == nil {
		return
	}
	if resp.Pattern.MatchedCap == "" {
		return
	}
	body := formatReplPatternSuggestion(*resp.Pattern, project)
	injectReminderIntoLastUserMessage(req, body)
}

// injectCapabilitiesTurn is the production entry point wired into the proxy
// pipeline. A thin wrapper over injectCapabilitiesTurnImpl that binds the
// queryFn to s.queryDaemon for live daemon RPC and passes the client's prompt
// profile, which selects the catalog dialect.
func (s *Server) injectCapabilitiesTurn(req map[string]any, threadID string, profile models.PromptProfile) bool {
	tidPreview := threadID
	if len(tidPreview) > 12 {
		tidPreview = tidPreview[:12]
	}
	if threadID == "" {
		log.Printf("[caps-inject] skip: empty threadID")
		return false
	}
	injected := injectCapabilitiesTurnImpl(req, threadID, s.getParentThread(threadID), profile, s.queryDaemon, s.capsCache, s.logger)
	// The preview is a slice of client text; without journalToken a newline inside
	// the first 12 bytes still forges a journal line.
	log.Printf("[caps-inject] threadID=%s injected=%v", journalToken(tidPreview), injected)
	return injected
}

func (s *Server) getParentThread(threadID string) string {
	return ""
}

// capCallableNames lists the script names a caller may pass as fn to
// execute_cap. Tool scripts are the intended surface; a cap that carries no
// tool script stays reachable by naming its internal scripts.
func capCallableNames(c CapInjection) []string {
	tools := make([]string, 0, len(c.Scripts))
	for _, sc := range c.Scripts {
		if sc.Kind == "tool" {
			tools = append(tools, sc.Name)
		}
	}
	if len(tools) > 0 {
		return tools
	}
	names := make([]string, 0, len(c.Scripts))
	for _, sc := range c.Scripts {
		names = append(names, sc.Name)
	}
	return names
}

// capTableCell renders one caller-defined text into a single cell of the catalog
// table. Script names and descriptions both come from save_cap without further
// filtering, so characters that would end the table row or the surrounding
// <caps-available> block are neutralized and the cell is bounded. One function for
// both cells on purpose: a later hardening must not reach one and miss the other.
func capTableCell(text string) string {
	cell := strings.NewReplacer(
		"|", "/", "<", "(", ">", ")", "\n", " ", "\r", " ", "\t", " ",
		// Unicode line separators: a renderer may break the row on them even though
		// they are not \n.
		"\u2028", " ", "\u2029", " ", "\u0085", " ",
	).Replace(text)
	cell = strings.TrimSpace(cell)
	// Truncate on a rune boundary: a byte cut through an umlaut yields invalid
	// UTF-8 in the injected block. Cutting at a rune start keeps the prefix as it
	// was — a cell that already carried a broken byte is shortened, not emptied.
	if len(cell) > 120 {
		cut := 117
		for cut > 0 && !utf8.RuneStart(cell[cut]) {
			cut--
		}
		cell = cell[:cut] + "..."
	}
	return cell
}

// capFunctionCell renders the Functions cell.
func capFunctionCell(c CapInjection) string {
	return capTableCell(strings.Join(capCallableNames(c), ", "))
}

// renderOpencodeCapabilitiesCatalog builds a catalog listing available caps
// for opencode sessions. opencode Code Mode has neither eval nor the Function
// constructor (measured 2026-10-17), so activate_cap cannot register a tool
// there: the catalog names execute_cap and the script each cap offers.
func renderOpencodeCapabilitiesCatalog(caps []CapInjection) string {
	if len(caps) == 0 {
		return ""
	}
	sorted := make([]CapInjection, len(caps))
	copy(sorted, caps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	b.WriteString("<system-reminder>\n<caps-available>\n")
	b.WriteString("# Available Capabilities\n\n")
	b.WriteString("To execute: in Code Mode `await tools.yesmem.execute_cap({name: \"<name>\", fn: \"<function>\", args: '{\"key\":\"value\"}'})`, as a direct MCP tool `yesmem_execute_cap(...)`. The daemon runs the script sandboxed: a script that names no sandbox profile inherits the daemon default (standard unless configured otherwise), and `none` applies only when the script writes it out. The result comes back as the return value.\n\n")
	b.WriteString("`fn` is one of the names in the Functions column; omit it only when the cap has a single tool script.\n\n")
	b.WriteString("When a capability script errors: diagnose the root cause, fix the script, save the corrected version via `save_cap` (`tools.yesmem.save_cap` in Code Mode), then retry.\n\n")
	b.WriteString("| Capability | Functions | Description |\n|---|---|---|\n")
	for _, cap := range sorted {
		fns := capFunctionCell(cap)
		b.WriteString(fmt.Sprintf("| %s | %s | %s |\n", capTableCell(cap.Name), fns, capTableCell(cap.Description)))
	}
	b.WriteString("</caps-available>\n</system-reminder>")
	return b.String()
}

// injectOpencodeCapabilitiesCatalog fetches active caps and injects the opencode catalog
// into the system prompt. Called from the OpenAI parity pipeline.
func (s *Server) injectOpencodeCapabilitiesCatalog(req map[string]any, threadID, project string) {
	if threadID == "" {
		return
	}
	qp := map[string]any{"thread_id": threadID}
	// Project scope: without it the daemon returns every project's caps.
	if project != "" {
		qp["project"] = project
	}
	raw, err := s.queryDaemon("get_active_caps", qp)
	if err != nil {
		return
	}
	caps, err := decodeCapsResponse(raw)
	if err != nil || len(caps) == 0 {
		return
	}
	catalog := renderOpencodeCapabilitiesCatalog(caps)
	if catalog == "" {
		return
	}
	AppendSystemBlock(req, "capabilities-available", catalog)
}

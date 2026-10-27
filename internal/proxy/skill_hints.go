package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Skill hint markers for injection/stripping
const (
	SkillHintStartMarker = "[skill-hint]"
	SkillHintEndMarker   = "[/skill-hint]"
	SkillEvalStartMarker = "[skill-eval]"
	SkillEvalEndMarker   = "[/skill-eval]"
)

var skillHintPattern = regexp.MustCompile(`(?s)\n?\[skill-hint\].*?\[/skill-hint\]`)
var skillEvalPattern = regexp.MustCompile(`(?s)\n?\[skill-eval\].*?\[/skill-eval\]`)

// skillHintTracker tracks which skills are active per thread.
//
// The thread id comes from a client header and is not validated, so the map is
// bounded: every entry would otherwise live for the process lifetime, one per
// invented id. Beyond the cap the least recently used thread loses its record,
// which costs it the post-collapse re-injection of its skill content — the same
// trade the decay tracker and the keepalive make.
type skillHintTracker struct {
	mu       sync.RWMutex
	active   map[string]map[string]bool // threadID → set of skill names
	lastUsed map[string]time.Time
}

// maxSkillThreads bounds the tracked threads (same order of magnitude as
// maxDecayTrackers and keepaliveMaxThreads).
const maxSkillThreads = 256

func newSkillHintTracker() *skillHintTracker {
	return &skillHintTracker{
		active:   make(map[string]map[string]bool),
		lastUsed: make(map[string]time.Time),
	}
}

func (t *skillHintTracker) markActive(threadID, skillName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active[threadID] == nil {
		t.active[threadID] = make(map[string]bool)
	}
	t.active[threadID][skillName] = true
	t.lastUsed[threadID] = time.Now()
	if len(t.active) > maxSkillThreads {
		t.evictLocked()
	}
}

// touch stamps a thread as seen without changing its skill set. The eviction
// order has to follow usage, not first activation: markActive runs only for a
// skill the thread did not have yet, so a long-lived session would otherwise be
// the oldest entry and lose its skills first (YM-349).
func (t *skillHintTracker) touch(threadID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.active[threadID]; !ok {
		return
	}
	t.lastUsed[threadID] = time.Now()
}

// evictLocked drops the least recently used threads down to the cap. Caller
// holds the write lock; the thread being marked has just been stamped, so it is
// never the first to go.
func (t *skillHintTracker) evictLocked() {
	type entry struct {
		threadID string
		used     time.Time
	}
	entries := make([]entry, 0, len(t.active))
	for id := range t.active {
		entries = append(entries, entry{id, t.lastUsed[id]})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
	for _, e := range entries[:len(entries)-maxSkillThreads] {
		delete(t.active, e.threadID)
		delete(t.lastUsed, e.threadID)
	}
}

func (t *skillHintTracker) isActive(threadID, skillName string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.active[threadID][skillName]
}

func (t *skillHintTracker) activeSkills(threadID string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var names []string
	for name := range t.active[threadID] {
		names = append(names, name)
	}
	return names
}

// stripSkillHints removes [skill-hint]...[/skill-hint] blocks from all messages
// except the last user message. This keeps old hints from wasting cache tokens.
func stripSkillHints(messages []any) []any {
	// Find last user message index
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "user" {
			lastUserIdx = i
			break
		}
	}

	result := make([]any, len(messages))
	for i, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok || i == lastUserIdx {
			result[i] = m
			continue
		}
		if msg["role"] != "user" {
			result[i] = m
			continue
		}

		// Strip skill hints from older user messages
		switch c := msg["content"].(type) {
		case string:
			stripped := skillHintPattern.ReplaceAllString(c, "")
			if stripped != c {
				newMsg := make(map[string]any, len(msg))
				for k, v := range msg {
					newMsg[k] = v
				}
				newMsg["content"] = strings.TrimSpace(stripped)
				result[i] = newMsg
			} else {
				result[i] = m
			}
		case []any:
			changed := false
			newBlocks := make([]any, 0, len(c))
			for _, block := range c {
				b, ok := block.(map[string]any)
				if !ok {
					newBlocks = append(newBlocks, block)
					continue
				}
				text, _ := b["text"].(string)
				if b["type"] == "text" && strings.Contains(text, SkillHintStartMarker) {
					stripped := skillHintPattern.ReplaceAllString(text, "")
					stripped = strings.TrimSpace(stripped)
					if stripped == "" {
						changed = true
						continue // remove empty block entirely
					}
					newBlock := make(map[string]any, len(b))
					for k, v := range b {
						newBlock[k] = v
					}
					newBlock["text"] = stripped
					newBlocks = append(newBlocks, newBlock)
					changed = true
				} else {
					newBlocks = append(newBlocks, block)
				}
			}
			if changed {
				newMsg := make(map[string]any, len(msg))
				for k, v := range msg {
					newMsg[k] = v
				}
				newMsg["content"] = newBlocks
				result[i] = newMsg
			} else {
				result[i] = m
			}
		default:
			result[i] = m
		}
	}
	return result
}

// skillEvalBlock is the mandatory instruction that forces Claude to evaluate
// available skills and commands against the current task. The skill/command list
// itself comes from Claude Code's system prompt — this only provides the trigger.
// Cached as package-level var to avoid per-request allocation.
var skillEvalBlock = SkillEvalStartMarker + "\n" +
	"INSTRUCTION: MANDATORY SKILL ACTIVATION SEQUENCE (on user text input only — skip on tool_result turns)\n" +
	"Step 1 — EVALUATE: For each available skill and /command listed above, decide: does it apply to the current task? Output format: list YES items as [name] YES — [reason]. Combine all NO items on a single line.\n" +
	"Step 2 — ACTIVATE: IF any YES → Use Skill(name) or /command NOW, before proceeding.\n" +
	"Step 3 — PROCEED: Only after evaluation is complete.\n" +
	SkillEvalEndMarker

// buildSkillEvalBlock returns the cached skill evaluation instruction block.
var silentSkillEvalBlock = SkillEvalStartMarker + "\n" +
	"ATTENTION — SKILL CHECK: If this task matches any available skill or /command → use Skill tool NOW. Otherwise proceed directly." +
	SkillEvalEndMarker

func buildSkillEvalBlock(mode string) string {
	switch mode {
	case "true":
		return skillEvalBlock
	case "false":
		return ""
	default:
		return silentSkillEvalBlock
	}
}

// detectSkillActivations scans message history for skill activations and
// returns the list of activated skill names. Both wire shapes count: the
// Anthropic tool_use block ({"type":"tool_use","name":"Skill","input":…}) and
// the OpenAI tool_calls shape an opencode or codex session sends
// ({"function":{"name":"Skill","arguments":"…"}}). Without the second shape
// those sessions were never tracked, so their skill content was never
// re-injected after a collapse (YM-349).
func detectSkillActivations(messages []any) []string {
	seen := make(map[string]bool)
	var skills []string

	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		for _, name := range anthropicSkillNames(msg["content"]) {
			if !seen[name] {
				seen[name] = true
				skills = append(skills, name)
			}
		}
		for _, name := range openAISkillNames(msg["tool_calls"]) {
			if !seen[name] {
				seen[name] = true
				skills = append(skills, name)
			}
		}
	}
	return skills
}

// anthropicSkillNames reads the skill names out of Anthropic content blocks.
func anthropicSkillNames(content any) []string {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var names []string
	for _, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if b["type"] != "tool_use" || b["name"] != "Skill" {
			continue
		}
		input, ok := b["input"].(map[string]any)
		if !ok {
			continue
		}
		if skillName, _ := input["skill"].(string); skillName != "" {
			names = append(names, skillName)
		}
	}
	return names
}

// openAISkillNames reads the skill names out of OpenAI tool_calls entries. The
// arguments are a JSON string; a malformed one names no skill. The tool name
// matches case-insensitively because opencode spells it "skill".
func openAISkillNames(raw any) []string {
	calls, ok := raw.([]any)
	if !ok {
		return nil
	}
	var names []string
	for _, call := range calls {
		c, ok := call.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := c["function"].(map[string]any)
		if !ok {
			continue
		}
		if toolName, _ := fn["name"].(string); !strings.EqualFold(toolName, "skill") {
			continue
		}
		args, _ := fn["arguments"].(string)
		var parsed map[string]any
		if args == "" || json.Unmarshal([]byte(args), &parsed) != nil {
			continue
		}
		// opencode names the field "name", the Anthropic-shaped call "skill".
		for _, key := range []string{"skill", "name"} {
			if skillName, _ := parsed[key].(string); skillName != "" {
				names = append(names, skillName)
				break
			}
		}
	}
	return names
}

// buildSkillInjectionBlock wraps skill content in markers for collapse protection.
func buildSkillInjectionBlock(name, content string) string {
	return fmt.Sprintf("[skill:%s]\n%s\n[/skill:%s]", name, content, name)
}

// syncSkillActivations scans messages for newly activated skills, marks them
// in the tracker, and loads their full content from the daemon for injection.
func (s *Server) syncSkillActivations(messages []any, project, threadID string) string {
	if threadID == "" || project == "" {
		return ""
	}

	detected := detectSkillActivations(messages)
	if len(detected) > 0 {
		// The thread is using its skills right now: keep it out of the eviction
		// order even when nothing new was activated.
		s.skillTracker.touch(threadID)
	}
	var newSkills []string
	for _, name := range detected {
		if !s.skillTracker.isActive(threadID, name) {
			s.skillTracker.markActive(threadID, name)
			newSkills = append(newSkills, name)
		}
	}

	if len(newSkills) == 0 {
		return ""
	}

	// Load full content for newly activated skills
	var blocks []string
	for _, name := range newSkills {
		result, err := s.queryDaemon("get_skill_content", map[string]any{
			"name":    name,
			"project": project,
		})
		if err != nil {
			continue
		}
		var resp struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(result, &resp); err != nil || resp.Content == "" {
			continue
		}
		blocks = append(blocks, buildSkillInjectionBlock(name, resp.Content))
	}

	if len(blocks) == 0 {
		return ""
	}
	return strings.Join(blocks, "\n\n")
}

package proxy

import (
	"encoding/json"
	"fmt"
	"testing"
)

// YM-349(2): the skill tracker's map is keyed by a thread id that comes from a
// client header and had no bound. One entry per distinct id lived for the
// process lifetime. The expected cap is the one the per-thread maps of this
// package use (maxDecayTrackers, keepaliveMaxThreads = 256); the test states it
// as a number so it fails on behaviour, not on a missing symbol.
func TestLos56c_SkillTrackerEvictsBeyondCap(t *testing.T) {
	const wantCap = 256
	tr := newSkillHintTracker()
	total := wantCap + 16
	for i := 0; i < total; i++ {
		tr.markActive(skillThreadForTest(i), "yesloop")
	}

	tr.mu.RLock()
	size := len(tr.active)
	tr.mu.RUnlock()
	if size > wantCap {
		t.Fatalf("tracker grew past its cap: %d threads (cap %d)", size, wantCap)
	}
	if tr.isActive(skillThreadForTest(0), "yesloop") {
		t.Errorf("oldest thread %q survived eviction", skillThreadForTest(0))
	}
	if !tr.isActive(skillThreadForTest(total-1), "yesloop") {
		t.Errorf("newest thread %q was dropped", skillThreadForTest(total-1))
	}
}

func skillThreadForTest(i int) string {
	return fmt.Sprintf("los56c-thread-%03d", i)
}

// YM-349(3): the detector knew only the Anthropic block shape
// (content: [{type: tool_use, name: Skill, input: {skill: ...}}]). An opencode
// or codex session speaks the OpenAI shape (content: null +
// tool_calls: [{function: {name, arguments}}]), so its skill activations were
// never tracked and the skill content was never re-injected after a collapse.
func TestLos56c_DetectSkillActivations_OpenAIToolCalls(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "Skill",
						"arguments": `{"skill":"yesloop"}`,
					},
				},
				map[string]any{
					"id":   "call_2",
					"type": "function",
					"function": map[string]any{
						"name":      "skill",
						"arguments": `{"name":"test-driven-development"}`,
					},
				},
			},
		},
	}

	got := detectSkillActivations(msgs)
	want := map[string]bool{"yesloop": true, "test-driven-development": true}
	if len(got) != len(want) {
		t.Fatalf("detected %v, want %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("detected unexpected skill %q (all: %v)", name, got)
		}
	}
}

// The Anthropic shape has to keep working, and a malformed arguments string
// must not be taken for an activation.
func TestLos56c_DetectSkillActivations_ShapesStayApart(t *testing.T) {
	anthropic := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "tool_use", "id": "tu1", "name": "Skill", "input": map[string]any{"skill": "brainstorming"}},
			},
		},
	}
	if got := detectSkillActivations(anthropic); len(got) != 1 || got[0] != "brainstorming" {
		t.Errorf("anthropic shape: got %v, want [brainstorming]", got)
	}

	broken := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"type": "function", "function": map[string]any{"name": "Skill", "arguments": "{nicht json"}},
				map[string]any{"type": "function", "function": map[string]any{"name": "Bash", "arguments": `{"skill":"nope"}`}},
			},
		},
	}
	if got := detectSkillActivations(broken); len(got) != 0 {
		t.Errorf("broken/foreign tool calls detected as skills: %v", got)
	}
}

// A tool_call whose arguments decode but name no skill must not be recorded
// under an empty name.
func TestLos56c_DetectSkillActivations_EmptySkillName(t *testing.T) {
	var args map[string]any
	if err := json.Unmarshal([]byte(`{"args":"x"}`), &args); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	msgs := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"type": "function", "function": map[string]any{"name": "Skill", "arguments": `{"args":"x"}`}},
			},
		},
	}
	if got := detectSkillActivations(msgs); len(got) != 0 {
		t.Errorf("empty skill name was recorded: %v", got)
	}
}

// YM-349(2), second round (cold review MEDIUM-1): markActive stamps the clock
// only when a skill is NEW for the thread, so the LRU order followed first
// activation and evicted exactly the long-lived thread. A session that keeps
// using its skills has to win against one-shot threads.
func TestLos56c_SkillTrackerKeepsFrequentlyUsedThreads(t *testing.T) {
	s := fakeDaemon(t, func(method string, params map[string]any) string {
		if method == "get_skill_content" {
			return `{"content":"skill body"}`
		}
		return "null"
	})
	s.skillTracker = newSkillHintTracker()

	skillMsgs := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "tool_use", "id": "tu1", "name": "Skill", "input": map[string]any{"skill": "yesloop"}},
			},
		},
	}

	const longLived = "los56c-long-lived"
	s.syncSkillActivations(skillMsgs, "/home/tester/memory/yesmem", longLived)

	// The same thread keeps working (every turn re-detects its active skill),
	// while 300 one-shot threads appear around it.
	for i := 0; i < 300; i++ {
		s.syncSkillActivations(skillMsgs, "/home/tester/memory/yesmem", longLived)
		s.syncSkillActivations(skillMsgs, "/home/tester/memory/yesmem", skillThreadForTest(i))
	}

	if !s.skillTracker.isActive(longLived, "yesloop") {
		t.Error("the long-lived thread lost its skill although it was still using it")
	}
}

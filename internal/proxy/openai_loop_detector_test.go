package proxy

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// TestOpenAIPipeline_LoopDetectionFires ensures CheckLoopAndFormat runs on the
// OpenAI-parity path so opencode-requests get loop warnings. Previously the
// warning was only wired into handleMessages (Anthropic path), so opencode
// sessions looping on identical tool calls got zero warnings.
func TestOpenAIPipeline_LoopDetectionFires(t *testing.T) {
	// 2 identical Edit→Bash cycles in OpenAI format. After translation these
	// become the canonical Anthropic tool_use/tool_result pattern that
	// detectIdenticalCycle triggers on.
	oaiReq := OpenAIChatRequest{
		Model:     "glm-5.2",
		MaxTokens: 1024,
		Messages: []OpenAIMessage{
			{Role: "user", Content: "fix it"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_1", Type: "function", Function: OpenAIFunctionCall{
					Name: "Edit", Arguments: `{"file_path":"main.go","old_string":"a","new_string":"b"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "edited"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_2", Type: "function", Function: OpenAIFunctionCall{
					Name: "Bash", Arguments: `{"command":"go test"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_2", Content: "ok"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_3", Type: "function", Function: OpenAIFunctionCall{
					Name: "Edit", Arguments: `{"file_path":"main.go","old_string":"a","new_string":"b"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_3", Content: "edited"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_4", Type: "function", Function: OpenAIFunctionCall{
					Name: "Bash", Arguments: `{"command":"go test"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_4", Content: "ok"},
		},
	}

	anthReq, err := translateOpenAIToAnthropic(oaiReq)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	s := &Server{
		cfg: Config{
			TokenThreshold:        200000,
			TokenMinimumThreshold: 80000,
			SawtoothEnabled:       false,
			SkillEvalInject:       "false",
			FeatureDefaults:       &config.FeatureGates{LoopWarning: true, Timestamps: true},
		},
		logger:         log.New(io.Discard, "", 0),
		loopStates:     map[string]*LoopState{},
		timestampStore: NewTimestampStore(),
	}

	ctx := openAIRequestContext{
		ReqIdx: 1,
		// A production opencode request carries its session header, so its
		// thread id names a session and the frozen per-message state is
		// allowed to carry per-session content (see annotateOpenAIMessageMetadata).
		ThreadID:        "opencode:test-session",
		Project:         "test",
		ThreadIsSession: true,
	}

	s.runOpenAIParityPipeline(anthReq, &ctx)

	if !hasLoopWarningInLastUserMessage(anthReq) {
		t.Errorf("expected [loop-warning] prefix in last user message; got messages: %+v", anthReq["messages"])
	}
	if hasLoopWarningInSystem(anthReq) {
		t.Errorf("loop warning must NOT be in system[] block anymore; got system: %+v", anthReq["system"])
	}
}

// TestOpenAIPipeline_LoopDetection_RetrySkipped verifies the gate: retry
// requests (ctx.Retry=true) must not run loop detection, mirroring the
// Anthropic path's !isRetryReq guard.
func TestOpenAIPipeline_LoopDetection_RetrySkipped(t *testing.T) {
	oaiReq := OpenAIChatRequest{
		Model:     "glm-5.2",
		MaxTokens: 1024,
		Messages: []OpenAIMessage{
			{Role: "user", Content: "fix it"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_1", Type: "function", Function: OpenAIFunctionCall{
					Name: "Edit", Arguments: `{"file_path":"main.go","old_string":"a","new_string":"b"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "edited"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_2", Type: "function", Function: OpenAIFunctionCall{
					Name: "Bash", Arguments: `{"command":"go test"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_2", Content: "ok"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_3", Type: "function", Function: OpenAIFunctionCall{
					Name: "Edit", Arguments: `{"file_path":"main.go","old_string":"a","new_string":"b"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_3", Content: "edited"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_4", Type: "function", Function: OpenAIFunctionCall{
					Name: "Bash", Arguments: `{"command":"go test"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_4", Content: "ok"},
		},
	}

	anthReq, err := translateOpenAIToAnthropic(oaiReq)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	s := &Server{
		cfg: Config{
			TokenThreshold:        200000,
			TokenMinimumThreshold: 80000,
			SawtoothEnabled:       false,
			SkillEvalInject:       "false",
			FeatureDefaults:       &config.FeatureGates{LoopWarning: true, Timestamps: true},
		},
		logger:         log.New(io.Discard, "", 0),
		loopStates:     map[string]*LoopState{},
		timestampStore: NewTimestampStore(),
	}

	ctx := openAIRequestContext{
		ReqIdx:   1,
		ThreadID: "opencode:test-session",
		Project:  "test",
		Retry:    true,
	}

	s.runOpenAIParityPipeline(anthReq, &ctx)

	if hasLoopWarningInLastUserMessage(anthReq) {
		t.Errorf("retry request must NOT inject loop warning; got last user message with warning")
	}
	if hasLoopWarningInSystem(anthReq) {
		t.Errorf("retry request must NOT inject loop warning into system[]; got system: %+v", anthReq["system"])
	}
}

// TestOpenAIPipeline_LoopDetection_NoLoopNoWarning verifies that a single
// tool call (no loop pattern) does not inject a warning — guards against
// false positives.
func TestOpenAIPipeline_LoopDetection_NoLoopNoWarning(t *testing.T) {
	oaiReq := OpenAIChatRequest{
		Model:     "glm-5.2",
		MaxTokens: 1024,
		Messages: []OpenAIMessage{
			{Role: "user", Content: "read the file"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_1", Type: "function", Function: OpenAIFunctionCall{
					Name: "Read", Arguments: `{"file_path":"main.go"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "package main"},
		},
	}

	anthReq, err := translateOpenAIToAnthropic(oaiReq)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	s := &Server{
		cfg: Config{
			TokenThreshold:        200000,
			TokenMinimumThreshold: 80000,
			SawtoothEnabled:       false,
			SkillEvalInject:       "false",
			FeatureDefaults:       &config.FeatureGates{LoopWarning: true, Timestamps: true},
		},
		logger:         log.New(io.Discard, "", 0),
		loopStates:     map[string]*LoopState{},
		timestampStore: NewTimestampStore(),
	}

	ctx := openAIRequestContext{
		ReqIdx:   1,
		ThreadID: "opencode:test-session",
		Project:  "test",
	}

	s.runOpenAIParityPipeline(anthReq, &ctx)

	if hasLoopWarningInLastUserMessage(anthReq) {
		t.Errorf("single tool call must not trigger loop warning; got last user message with warning")
	}
}

// hasLoopWarningInLastUserMessage checks whether the last user/tool message
// in the request contains a [loop-warning] prefix. This is the new location
// for loop warnings after the cache-safe migration from system[] blocks.
func hasLoopWarningInLastUserMessage(req map[string]any) bool {
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	last, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok {
		return false
	}
	role, _ := last["role"].(string)
	if role != "user" && role != "tool" {
		return false
	}
	switch content := last["content"].(type) {
	case string:
		return strings.Contains(content, "[loop-warning]")
	case []any:
		for _, block := range content {
			bm, ok := block.(map[string]any)
			if !ok || bm["type"] != "text" {
				continue
			}
			text, _ := bm["text"].(string)
			if strings.Contains(text, "[loop-warning]") {
				return true
			}
		}
	}
	return false
}

// hasLoopWarningInSystem checks whether the system[] array contains a loop
// warning block. After the migration, this must always return false.
func hasLoopWarningInSystem(req map[string]any) bool {
	blocks, _ := req["system"].([]any)
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		text, _ := bm["text"].(string)
		if strings.Contains(text, "yesmem-loop-warning") ||
			strings.Contains(text, "[loop-warning]") ||
			strings.Contains(text, "[YesMem Loop Detection]") {
			return true
		}
	}
	return false
}

// hasMetaInMessage checks whether a given message contains a metadata marker
// prefix (e.g. "[plan-checkpoint]", "[docs-hint]").
func hasMetaInMessage(msg map[string]any, marker string) bool {
	switch content := msg["content"].(type) {
	case string:
		return strings.Contains(content, marker)
	case []any:
		for _, block := range content {
			bm, ok := block.(map[string]any)
			if !ok || bm["type"] != "text" {
				continue
			}
			text, _ := bm["text"].(string)
			if strings.Contains(text, marker) {
				return true
			}
		}
	}
	return false
}

// TestInjectTimestamps_WithPlanCheckpoint verifies that the InjectTimestamps
// replay path renders [plan-checkpoint] and [docs-hint] markers from a frozen
// TimestampMeta entry. This is the core freeze-once-replay mechanism.
func TestInjectTimestamps_WithPlanCheckpointAndDocsHint(t *testing.T) {
	ts := NewTimestampStore()
	ts.Store("t1", 1, &TimestampMeta{
		Timestamp:      "Sa 2026-07-04 12:00:00",
		Delta:          "4s",
		PlanCheckpoint: "[Plan Checkpoint]\nUpdate your plan.\n[/Plan Checkpoint]",
		DocsHint:       "Du hast 15 indexed docs verfügbar.",
	})

	msgs := []any{
		map[string]any{"role": "user", "content": "first question"},
	}

	n := InjectTimestamps(ts, "t1", msgs, 1, 0, 0)
	if n != 1 {
		t.Fatalf("expected 1 injection, got %d", n)
	}

	if !hasMetaInMessage(msgs[0].(map[string]any), "[plan-checkpoint]") {
		t.Errorf("missing [plan-checkpoint] marker")
	}
	if !hasMetaInMessage(msgs[0].(map[string]any), "[docs-hint]") {
		t.Errorf("missing [docs-hint] marker")
	}
}

// TestInjectTimestamps_PlanCheckpointAlongsideLoopWarning verifies that both
// markers coexist in the rendered metadata when the frozen entry has both.
func TestInjectTimestamps_PlanCheckpointAlongsideLoopWarning(t *testing.T) {
	ts := NewTimestampStore()
	ts.Store("t1", 1, &TimestampMeta{
		Timestamp:      "Sa 2026-07-04 12:00:00",
		LoopWarning:    "[YesMem Loop Detection] looping",
		PlanCheckpoint: "[Plan Checkpoint] Update[/Plan Checkpoint]",
	})

	msgs := []any{
		map[string]any{"role": "user", "content": "question"},
	}

	n := InjectTimestamps(ts, "t1", msgs, 1, 0, 0)
	if n != 1 {
		t.Fatalf("expected 1 injection, got %d", n)
	}

	if !hasMetaInMessage(msgs[0].(map[string]any), "[loop-warning]") {
		t.Errorf("missing [loop-warning] marker")
	}
	if !hasMetaInMessage(msgs[0].(map[string]any), "[plan-checkpoint]") {
		t.Errorf("missing [plan-checkpoint] marker")
	}
}

// TestInjectTimestamps_ReplayPreservesPlanCheckpoint verifies the freeze-once
// contract: a plan checkpoint seeded at msg:3 persists across requests and is
// replayed on follow-up requests that annotate earlier messages.
func TestInjectTimestamps_ReplayPreservesPlanCheckpoint(t *testing.T) {
	ts := NewTimestampStore()
	ts.Store("t1", 3, &TimestampMeta{
		Timestamp:      "Sa 2026-07-04 12:00:00",
		Delta:          "5s",
		PlanCheckpoint: "[Plan Checkpoint] Persistent[/Plan Checkpoint]",
	})

	// Simulate follow-up request: msg:1 (pass through), msg:2 (pass through), msg:3 (has frozen checkpoint)
	msgs := []any{
		map[string]any{"role": "user", "content": "old question"},     // msg:1
		map[string]any{"role": "assistant", "content": "old answer"},  // msg:2 (skip)
		map[string]any{"role": "user", "content": "new question"},     // msg:3 — has frozen checkpoint
		map[string]any{"role": "assistant", "content": "new answer"},  // msg:4 (skip)
		map[string]any{"role": "user", "content": "current question"}, // msg:5 — current
	}

	// Inject on positions 0-3 (exclude current = position 4)
	n := InjectTimestamps(ts, "t1", msgs, 4, 0, 0)
	if n != 4 {
		t.Fatalf("expected 4 injections, got %d", n)
	}

	msg3 := msgs[2].(map[string]any)
	if !hasMetaInMessage(msg3, "[plan-checkpoint]") {
		t.Errorf("frozen PlanCheckpoint not replayed at msg:3")
	}
}

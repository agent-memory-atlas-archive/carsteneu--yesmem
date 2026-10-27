package proxy

import (
	"io"
	"log"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// los36LoopingOpenAIRequest is an identical Edit→Bash cycle in OpenAI format.
// After translation it is the Anthropic tool_use/tool_result pattern that
// DetectLoop recognizes.
func los36LoopingOpenAIRequest() OpenAIChatRequest {
	return OpenAIChatRequest{
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
}

func los36LoopServer() *Server {
	return &Server{
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
}

// YM-132: loop detection ran before the subagent decision. A subagent request
// therefore raised the parent thread's warning counter without ever receiving
// the warning (the subagent branch returns before the injection). A subagent
// must leave the loop state untouched.
func TestOpenAIPipeline_LoopDetection_SkipsSubagent(t *testing.T) {
	anthReq, err := translateOpenAIToAnthropic(los36LoopingOpenAIRequest())
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	s := los36LoopServer()
	ctx := openAIRequestContext{
		ReqIdx:   1,
		ThreadID: "opencode:test-subagent",
		Project:  "test",
		Subagent: true,
	}

	s.runOpenAIParityPipeline(anthReq, &ctx)

	if hasLoopWarningInLastUserMessage(anthReq) {
		t.Error("a subagent request must not carry a loop warning")
	}
	if state := s.loopStates["opencode:test-subagent"]; state != nil && state.WarningCount != 0 {
		t.Errorf("a subagent request must not raise the loop warning count, got %d", state.WarningCount)
	}
}

// The guard must not disable loop detection for real sessions: the same
// request without the subagent flag still warns.
func TestOpenAIPipeline_LoopDetection_FiresForMainSession(t *testing.T) {
	anthReq, err := translateOpenAIToAnthropic(los36LoopingOpenAIRequest())
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	s := los36LoopServer()
	ctx := openAIRequestContext{
		ReqIdx: 1,
		// Session-keyed like a real opencode request, so the frozen loop warning
		// may be replayed (see annotateOpenAIMessageMetadata).
		ThreadID:        "opencode:test-main",
		Project:         "test",
		ThreadIsSession: true,
	}

	s.runOpenAIParityPipeline(anthReq, &ctx)

	if !hasLoopWarningInLastUserMessage(anthReq) {
		t.Error("a main-session loop must still produce a warning")
	}
}

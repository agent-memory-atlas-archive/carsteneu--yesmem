package proxy

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// YM-290 (BRF-I2): the Anthropic path runs syncSkillActivations outside the
// skill_eval gate (los27_skill_eval_gate_test.go); the OpenAI-parity path still
// called it only when the gate was on, so activity was never tracked on that
// wire while the gate was off. The tracker is the only source for re-injecting
// skill content after a context collapse.
//
// The call drives the Anthropic tool_use shape because that is the only shape
// detectSkillActivations recognizes (pre-existing, reported separately) — the
// gate placement is what this test pins.
func los27bTrackerServer() *Server {
	return &Server{
		cfg:                Config{FeatureDefaults: &config.FeatureGates{Timestamps: true, SkillEval: false}},
		timestampStore:     NewTimestampStore(),
		logger:             log.New(io.Discard, "", 0),
		skillTracker:       newSkillHintTracker(),
		channelInjectCount: map[string]int{},
	}
}

func los27bSkillMessages() []any {
	return []any{
		map[string]any{"role": "user", "content": "start the task"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tu1", "name": "Skill", "input": map[string]any{"skill": "test-driven-development"}},
		}},
		map[string]any{"role": "user", "content": "continue"},
	}
}

func TestLos27b_OpenAIPathTracksSkillActivationsWithGateOff(t *testing.T) {
	s := los27bTrackerServer()

	const threadID = "ses_los27b_tracker"
	msgs := los27bSkillMessages()
	req := map[string]any{"messages": msgs}
	ctx := &openAIRequestContext{ThreadID: threadID, Project: "proj", ThreadIsSession: true, ReqIdx: 1, RawMsgCount: 3}

	s.annotateOpenAIMessageMetadata(req, ctx, msgs, "glm-5.2", 1000)

	if !s.skillTracker.isActive(threadID, "test-driven-development") {
		t.Error("skill activation was not tracked while the skill-eval gate was off")
	}
	// The gate still governs the instruction block itself.
	for _, m := range msgs {
		if got, _ := m.(map[string]any)["content"].(string); strings.Contains(got, SkillEvalStartMarker) {
			t.Errorf("skill-eval block was injected although the gate is off: %q", got)
		}
	}
}

// A codex thread id is derived from message content and shared by every session
// of one client build, so tracking under it would replay one session's skills
// into another. The channel block guards this with ThreadIsSession; the tracker
// call must do the same.
func TestLos27b_DerivedThreadIDIsNotTracked(t *testing.T) {
	s := los27bTrackerServer()

	const derivedThread = "codex:0123456789abcdef"
	msgs := los27bSkillMessages()
	req := map[string]any{"messages": msgs}
	ctx := &openAIRequestContext{ThreadID: derivedThread, Project: "proj", ThreadIsSession: false, ReqIdx: 1, RawMsgCount: 3}

	s.annotateOpenAIMessageMetadata(req, ctx, msgs, "gpt-5.5", 1000)

	if s.skillTracker.isActive(derivedThread, "test-driven-development") {
		t.Error("a content-derived thread id must not accumulate skill activations")
	}
}

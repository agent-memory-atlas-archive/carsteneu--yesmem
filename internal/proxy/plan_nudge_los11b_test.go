package proxy

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// los11bPlanReadMessages builds the shape annotateOpenAIMessageMetadata sees on
// the parity path: a Read tool call on a plan file inside the scan window, then
// a fresh user turn (the annotator only touches a trailing user/tool message).
func los11bPlanReadMessages(path string) []any {
	return []any{
		map[string]any{"role": "user", "content": "weiter"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "tool_los11b",
				"name":  "read",
				"input": map[string]any{"path": path},
			},
		}},
		map[string]any{"role": "tool", "content": "plan body"},
		map[string]any{"role": "user", "content": "mach weiter"},
	}
}

// los11bDaemon answers get_plan with "no plan" and everything else with an empty
// result, so the parity pipeline runs hermetically against a stub socket.
func los11bDaemon(method string, params map[string]any) string {
	if method == "get_plan" {
		return `{"exists":false,"plan":"","status":"","docs_hint":""}`
	}
	return "null"
}

func los11bAnnotateServer(t *testing.T, daemon bool) *Server {
	t.Helper()
	var s *Server
	if daemon {
		s = fakeDaemon(t, los11bDaemon)
	} else {
		// No DataDir: queryDaemon fails, which is the "daemon did not answer" case.
		s = &Server{}
	}
	s.timestampStore = NewTimestampStore()
	s.channelInjectCount = map[string]int{}
	s.logger = log.New(io.Discard, "", 0)
	return s
}

// los11bCtx is a session-keyed thread (the normal opencode case). ThreadIsSession
// decides whether the nudge may be frozen into the per-message store.
func los11bCtx(threadID string, rawMsgCount int) *openAIRequestContext {
	return &openAIRequestContext{
		ThreadID:        threadID,
		ThreadIsSession: true,
		Project:         "/repo",
		ReqIdx:          1,
		RawMsgCount:     rawMsgCount,
	}
}

func los11bContent(t *testing.T, msgs []any, idx int) string {
	t.Helper()
	msg, ok := msgs[idx].(map[string]any)
	if !ok {
		t.Fatalf("message %d is not a map: %T", idx, msgs[idx])
	}
	content, _ := msg["content"].(string)
	return content
}

// YM-252 (PRX-P29): the plan auto-activation nudge ran only on the Anthropic
// path (proxy.go), so an opencode or codex session that read an implementation
// plan was never asked to call set_plan(). The parity path now carries the nudge
// through the per-message store, which keeps the cached prefix valid.
func TestOpenAIPlanNudge_Los11b(t *testing.T) {
	s := los11bAnnotateServer(t, true)
	msgs := los11bPlanReadMessages("/repo/docs/plan.md")
	req := map[string]any{"messages": msgs}
	ctx := los11bCtx("ses_plan_nudge", 4)

	s.annotateOpenAIMessageMetadata(req, ctx, msgs, "glm-5.2", 1000)

	last := los11bContent(t, msgs, len(msgs)-1)
	if !strings.Contains(last, "[plan-nudge]") {
		t.Fatalf("no plan nudge on the parity path, last message = %q", last)
	}
	if !strings.Contains(last, "set_plan()") {
		t.Errorf("nudge does not name the tool it asks for: %q", last)
	}
	if !strings.Contains(last, "plan.md") {
		t.Errorf("nudge does not name the plan file: %q", last)
	}
}

// The nudge is a stable inject: it is frozen once into msg:N and afterwards
// replayed from the store, so a later turn carries it without writing it again.
// This drives the real replay path (InjectTimestamps), not just the freeze.
func TestOpenAIPlanNudge_ReplayedFromStore_Los11b(t *testing.T) {
	s := los11bAnnotateServer(t, true)
	// Keep the fake daemon's DataDir; only enable the timestamps gate so the
	// replay path (InjectTimestamps) actually runs.
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true}
	const thread = "ses_plan_replay"

	// Turn 1: the read is the fourth message; the nudge is frozen there.
	turn1 := los11bPlanReadMessages("/repo/docs/plan.md")
	s.annotateOpenAIMessageMetadata(map[string]any{"messages": turn1}, los11bCtx(thread, 4), turn1, "glm-5.2", 1000)
	if got := los11bContent(t, turn1, 3); !strings.Contains(got, "[plan-nudge]") {
		t.Fatalf("turn 1 did not annotate the read turn: %q", got)
	}

	// Turn 2: the client sends the raw history again plus two more turns. The
	// read is now outside the four-message window, so nothing new is detected —
	// the nudge on msg:4 must come from the store.
	turn2 := []any{
		map[string]any{"role": "user", "content": "weiter"},
		turn1[1],
		map[string]any{"role": "tool", "content": "plan body"},
		map[string]any{"role": "user", "content": "mach weiter"},
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "und jetzt?"},
	}
	s.annotateOpenAIMessageMetadata(map[string]any{"messages": turn2}, los11bCtx(thread, 6), turn2, "glm-5.2", 1000)

	if got := los11bContent(t, turn2, 3); !strings.Contains(got, "[plan-nudge]") {
		t.Fatalf("the frozen nudge was not replayed from the store: %q", got)
	}
	if got := los11bContent(t, turn2, 5); strings.Contains(got, "[plan-nudge]") {
		t.Fatalf("the nudge was written a second time instead of replayed: %q", got)
	}

	meta, ok := s.timestampStore.Get(thread, 4)
	if !ok || meta.PlanNudge == "" {
		t.Fatalf("msg:4 carries no stored nudge: ok=%t meta=%+v", ok, meta)
	}
	if meta6, ok := s.timestampStore.Get(thread, 6); ok && meta6 != nil && meta6.PlanNudge != "" {
		t.Fatalf("msg:6 duplicated the nudge: %+v", meta6)
	}
}

// A codex thread id is derived from message content, so several sessions share
// it. Freezing the nudge there would replay "you just read a plan" into a
// session that never did — the channel block 50 lines below is not frozen for
// the same reason. The nudge rides the tail message instead.
func TestOpenAIPlanNudge_NotFrozenForDerivedThreadIDs_Los11b(t *testing.T) {
	s := los11bAnnotateServer(t, true)
	msgs := los11bPlanReadMessages("/repo/docs/plan.md")
	ctx := los11bCtx("codex:derived-hash", 4)
	ctx.ThreadIsSession = false

	s.annotateOpenAIMessageMetadata(map[string]any{"messages": msgs}, ctx, msgs, "glm-5.2", 1000)

	if got := los11bContent(t, msgs, len(msgs)-1); !strings.Contains(got, "[plan-nudge]") {
		t.Fatalf("the nudge must still ride the tail message: %q", got)
	}
	if meta, ok := s.timestampStore.Get("codex:derived-hash", 4); ok && meta != nil && meta.PlanNudge != "" {
		t.Fatalf("the nudge was frozen into a shared thread id: %+v", meta)
	}
}

// A read of any ordinary source file must not produce the nudge.
func TestOpenAIPlanNudge_IgnoresPlainSourceFiles_Los11b(t *testing.T) {
	s := los11bAnnotateServer(t, true)
	msgs := los11bPlanReadMessages("/repo/internal/proxy/proxy.go")
	req := map[string]any{"messages": msgs}
	ctx := los11bCtx("ses_plan_nudge_neg", 4)

	s.annotateOpenAIMessageMetadata(req, ctx, msgs, "glm-5.2", 1000)

	if last := los11bContent(t, msgs, len(msgs)-1); strings.Contains(last, "[plan-nudge]") {
		t.Fatalf("nudge fired on a non-plan file: %q", last)
	}
}

// "No active plan" and "the daemon did not answer" are different facts. The
// nudge text claims the plan is inactive, and on this path it is frozen into the
// message for the rest of the thread, so a silent daemon must not produce it.
func TestOpenAIPlanNudge_SkipsWhenDaemonIsSilent_Los11b(t *testing.T) {
	s := los11bAnnotateServer(t, false)
	msgs := los11bPlanReadMessages("/repo/docs/plan.md")
	ctx := los11bCtx("ses_plan_silent", 4)

	s.annotateOpenAIMessageMetadata(map[string]any{"messages": msgs}, ctx, msgs, "glm-5.2", 1000)

	if last := los11bContent(t, msgs, len(msgs)-1); strings.Contains(last, "[plan-nudge]") {
		t.Fatalf("nudge fired although get_plan never answered: %q", last)
	}
}

// The rendering of the field is what actually reaches the prompt; the parity
// tests above go through it, this one pins the line format on its own.
func TestBuildMeta_PlanNudge_Los11b(t *testing.T) {
	got := BuildMeta(3, &TimestampMeta{PlanNudge: "activate it via set_plan()"})
	if !strings.Contains(got, "[plan-nudge] activate it via set_plan()") {
		t.Fatalf("BuildMeta = %q, want a [plan-nudge] line", got)
	}
}

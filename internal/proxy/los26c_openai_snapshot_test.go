package proxy

import (
	"io"
	"log"

	"testing"
	"time"
)

// TestLos26c_OpenAIPathRefreshesFrozenSnapshot pins finding 3 of YM-315. The
// Anthropic pipeline re-snapshots the frozen prefix after the per-message
// annotation stage; the OpenAI-parity path never did, so its stored prefix kept
// the pre-annotation bytes while the wire carried the annotated ones.
//
// The mutation is production-reachable, not synthetic: the "last assistant
// message" timestamp pass walks back to the newest assistant turn, and when the
// tail of the request holds only user messages that turn lies inside the frozen
// prefix.
func TestLos26c_OpenAIPathRefreshesFrozenSnapshot(t *testing.T) {
	const tid = "opencode:ses_los26c_snapshot"
	s := &Server{
		timestampStore:     NewTimestampStore(),
		logger:             log.New(io.Discard, "", 0),
		frozenStubs:        NewFrozenStubs(),
		channelInjectCount: map[string]int{},
	}

	frozen := []any{
		map[string]any{"role": "user", "content": "frozen one"},
		map[string]any{"role": "assistant", "content": "frozen two"},
	}
	s.frozenStubs.Store(tid, frozen, 4, frozen[1], 100, 100)

	msgs := []any{
		map[string]any{"role": "user", "content": "frozen one"},
		map[string]any{"role": "assistant", "content": "frozen two"},
		map[string]any{"role": "user", "content": "tail three"},
		map[string]any{"role": "user", "content": "tail four"},
	}
	req := map[string]any{"messages": msgs}
	ctx := &openAIRequestContext{ThreadID: tid, Project: "proj", ReqIdx: 1, RawMsgCount: 4, SawtoothCutoff: 4, SawtoothStubs: 2}

	// A response time exists, so the annotation pass stamps the newest assistant
	// turn — which is message 1, inside the frozen range.
	s.responseTimes = map[string]time.Time{tid: time.Now()}

	s.annotateOpenAIMessageMetadata(req, ctx, msgs, "glm-5.2", 1000)

	wireMsgs, _ := req["messages"].([]any)
	wireAssistant, _ := wireMsgs[1].(map[string]any)
	wireContent, _ := wireAssistant["content"].(string)
	if !msgHasMetaPrefix(wireAssistant) {
		t.Fatalf("test premise broken: the wire bytes of the frozen range were not annotated: %q", wireContent)
	}

	stored := s.frozenStubs.Get(tid, wireMsgs)
	if stored == nil {
		t.Fatal("no frozen prefix after the annotation pass")
	}
	storedAssistant, _ := stored.Messages[1].(map[string]any)
	storedContent, _ := storedAssistant["content"].(string)
	if storedContent != wireContent {
		t.Fatalf("stored frozen prefix differs from the wire bytes:\nstored %q\nwire   %q", storedContent, wireContent)
	}
}

// TestLos26c_SnapshotStoresBreakpointFreePrefix pins the invariant the freeze
// path establishes and the snapshot used to break: the stored prefix carries no
// cache_control, the outgoing request keeps its breakpoints. The OpenAI-parity
// pipeline has no restore-side strip, so a breakpoint stored there would ride
// into every later upstream request.
func TestLos26c_SnapshotStoresBreakpointFreePrefix(t *testing.T) {
	const tid = "opencode:ses_los26c_clean_prefix"
	s := &Server{
		timestampStore:     NewTimestampStore(),
		logger:             log.New(io.Discard, "", 0),
		frozenStubs:        NewFrozenStubs(),
		channelInjectCount: map[string]int{},
	}

	frozen := []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "frozen one"},
		},
	}}
	s.frozenStubs.Store(tid, frozen, 3, frozen[0], 100, 100)

	// The request of this turn carries a client breakpoint in the frozen range.
	msgs := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":          "text",
					"text":          "frozen one",
					"cache_control": map[string]any{"type": "ephemeral"},
				},
			},
		},
		map[string]any{"role": "user", "content": "tail three"},
		map[string]any{"role": "user", "content": "tail four"},
	}
	req := map[string]any{"messages": msgs}
	ctx := &openAIRequestContext{ThreadID: tid, Project: "proj", ReqIdx: 1, RawMsgCount: 3, SawtoothCutoff: 3, SawtoothStubs: 1}

	s.annotateOpenAIMessageMetadata(req, ctx, msgs, "glm-5.2", 1000)

	wireMsgs, _ := req["messages"].([]any)
	wireMsg, _ := wireMsgs[0].(map[string]any)
	wireBlocks, _ := wireMsg["content"].([]any)
	if len(wireBlocks) == 0 {
		t.Fatal("test premise broken: the request lost its content blocks")
	}
	if _, has := wireBlocks[0].(map[string]any)["cache_control"]; !has {
		t.Fatal("the snapshot stripped the breakpoint from the outgoing request")
	}

	stored := s.frozenStubs.Get(tid, wireMsgs)
	if stored == nil {
		t.Fatal("no frozen prefix after the annotation pass")
	}
	storedMsg, _ := stored.Messages[0].(map[string]any)
	storedBlocks, _ := storedMsg["content"].([]any)
	if len(storedBlocks) == 0 {
		t.Fatal("stored prefix lost its content blocks")
	}
	if _, has := storedBlocks[0].(map[string]any)["cache_control"]; has {
		t.Fatal("the stored frozen prefix carries a cache_control marker: every later request starts from a prefix the freeze path deliberately keeps clean")
	}
}

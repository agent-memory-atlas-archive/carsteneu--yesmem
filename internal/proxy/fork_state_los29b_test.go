package proxy

import (
	"net/http"
	"testing"
)

// Regression YM-272, part 3: a Claude Code subagent runs under the parent
// session's thread ID (isSubagentFromBody, agent_type=subagent), so its
// responses fed the parent's fork state. They advanced lastTotalTokens to the
// subagent's absolute count, which swallowed the parent's token growth, and they
// counted toward maxForksPerSession, so subagent work spent the parent's fork
// budget.
func TestForkState_SubagentDoesNotConsumeParentForkBudget(t *testing.T) {
	const tid = "ses_los29bParent"
	fs := NewForkState(20000, 60000, 3, 1)

	// The subagent reports a large absolute input count for the same thread.
	if fs.ShouldFork(tid, 200000, true, true) {
		t.Fatal("a subagent response fired a fork of the parent session")
	}
	// The parent's own growth still counts from the parent's baseline: the
	// subagent number must not have moved it.
	if !fs.ShouldFork(tid, 200000, true, false) {
		t.Fatal("the parent's own request missed the fork trigger, the subagent stole its baseline")
	}

	fs.RecordFork(tid)
	if fs.ShouldFork(tid, 400000, true, false) {
		t.Fatal("more forks were granted than maxForksPerSession allows")
	}
}

// The fork gate must never see a marker the passthrough decision does not know:
// a request that is a declared subagent leaves handleMessages through the
// subagent branch, so the narrowed marker set is only useful while it stays a
// subset of isSubagentRequest.
func TestIsDeclaredSubagent_IsASubsetOfIsSubagentRequest(t *testing.T) {
	subagentBody := map[string]any{
		"metadata": map[string]any{"user_id": `{"agent_type":"subagent"}`},
	}
	sdkBody := map[string]any{
		"system": []any{map[string]any{"text": "… cc_entrypoint=sdk-ts …"}},
	}
	haikuBody := map[string]any{"model": "claude-haiku-4-5"}

	cases := []struct {
		name string
		h    http.Header
		req  map[string]any
	}{
		{name: "metadata agent_type", h: http.Header{}, req: subagentBody},
		{name: "sdk entrypoint", h: http.Header{}, req: sdkBody},
		{name: "haiku model", h: http.Header{}, req: haikuBody},
		{name: "agent type header", h: http.Header{"X-Opencode-Agent-Type": {"subagent"}}, req: nil},
		{name: "parent session header", h: http.Header{"X-Opencode-Parent-Session": {"ses_parent"}}, req: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !isDeclaredSubagent(c.h, c.req) {
				t.Fatal("marker not detected, the case does not test the invariant")
			}
			msgs := []any{map[string]any{"role": "user", "content": "hi"}}
			if !isSubagentRequest(c.h, msgs, c.req) {
				t.Fatal("declared subagent is not classified as a subagent request: it would reach the fork gate on the wrong path")
			}
		})
	}
}

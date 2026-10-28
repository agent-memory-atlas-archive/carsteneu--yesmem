package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for Los 26b (YM-263, PRX-P31). Findings escaped from Los 26a.
// Helpers and test names carry the los26b prefix so parallel loops in this
// package cannot collide on a name.

// --- K1: absent thinking is not evidence of a subagent -----------------------

// los26bMainReqNoThinking is a CLI main session that requests no extended
// thinking. The billing header is the client's own statement about the
// entrypoint, so it must outrank the missing thinking field.
func los26bMainReqNoThinking() map[string]any {
	return map[string]any{
		"model": "claude-opus-4-6",
		"system": []any{
			map[string]any{"text": "x-anthropic-billing-header: cc_version=2.1.88.cfc; cc_entrypoint=cli; cch=00000;"},
		},
	}
}

// TestLos26b_NonThinkingMainSessionKeepsThePipeline pins the fix for the
// escaped finding: isSubagent classified EVERY request without a thinking field
// as a subagent, so clients and models without extended thinking lost briefing,
// stubs, collapse and the emergency brake entirely.
func TestLos26b_NonThinkingMainSessionKeepsThePipeline(t *testing.T) {
	msgs := buildLargeMessages(5)

	t.Run("cli-entrypoint-without-thinking", func(t *testing.T) {
		if isSubagentRequest(http.Header{}, msgs, los26bMainReqNoThinking()) {
			t.Fatal("main session (cc_entrypoint=cli, no thinking) was classified as subagent; the whole pipeline stays off for non-thinking clients")
		}
	})

	t.Run("opencode-session-without-thinking", func(t *testing.T) {
		req := map[string]any{"model": "glm-5.2"}
		h := http.Header{}
		h.Set("x-opencode-session-id", "ses_los26b_main")
		if isSubagentRequest(h, msgs, req) {
			t.Fatal("opencode main session (session header, no thinking) was classified as subagent")
		}
	})

	t.Run("legacy-affinity-header-without-thinking", func(t *testing.T) {
		// Pre-fork builds send x-session-affinity instead of x-opencode-session-id.
		req := map[string]any{"model": "glm-5.2"}
		h := http.Header{}
		h.Set("x-session-affinity", "ses_los26b_old")
		if isSubagentRequest(h, msgs, req) {
			t.Fatal("opencode main session (affinity header, no thinking) was classified as subagent")
		}
	})

	// Counter-checks: the markers that really identify a subagent must keep
	// working, otherwise the fix trades one blind spot for another.
	t.Run("parent-header-still-wins", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Opencode-Parent-Session-Id", "ses_parent")
		if !isSubagentRequest(h, msgs, los26bMainReqNoThinking()) {
			t.Fatal("parent-session header no longer identifies a subagent")
		}
	})

	t.Run("sdk-ts-still-detected", func(t *testing.T) {
		req := map[string]any{
			"model": "claude-opus-4-6",
			"system": []any{
				map[string]any{"text": "x-anthropic-billing-header: cc_entrypoint=sdk-ts; cch=00000;"},
			},
		}
		h := http.Header{}
		h.Set("x-opencode-session-id", "ses_los26b_child")
		if !isSubagentRequest(h, msgs, req) {
			t.Fatal("sdk-ts entrypoint no longer identifies a subagent")
		}
	})

	t.Run("unmarked-client-still-falls-back", func(t *testing.T) {
		req := map[string]any{"model": "claude-opus-4-6"}
		if !isSubagentRequest(http.Header{}, msgs, req) {
			t.Fatal("request without any client marker and without thinking lost the subagent fallback")
		}
	})
}

// --- K2: a cache-only response carries usage too -----------------------------

// TestLos26b_CacheOnlyResponseReachesSawtooth pins the fix for the gate
// `jsonResp.Usage.InputTokens > 0`: on a pure cache hit input_tokens is 0, so
// the whole non-streaming block (sawtooth update, usage tracking, cache status
// writer, TTL detector, keepalive reset, response timestamp) was skipped.
func TestLos26b_CacheOnlyResponseReachesSawtooth(t *testing.T) {
	var received []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"usage":{"input_tokens":0,"output_tokens":5,"cache_read_input_tokens":50000}}`))
	}))
	defer backend.Close()

	s := los26aHandlerServer(t, backend.URL)
	s.cfg.SawtoothEnabled = true

	body := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},"messages":[` +
		`{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-opencode-session-id", "ses_los26b_cache")
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if received == nil {
		t.Fatal("request never reached the upstream")
	}
	const threadID = "opencode:ses_los26b_cache"
	if got := s.sawtoothTrigger.GetLastTokens(threadID); got != 50000 {
		t.Fatalf("lastTotalTokens = %d, want 50000 (input_tokens=0 plus 50000 cached read); a gate on input_tokens alone drops pure cache responses", got)
	}
}

// --- K3: the state load must not hold the trigger lock over a daemon RPC -----

// TestLos26b_LoadFromDBDoesNotBlockOtherThreads pins the fix for loadFromDB
// holding the write lock across loadFn. loadFn is a daemon round trip (up to
// 15s on a cold start), so every other thread's ShouldTrigger stalled behind it.
func TestLos26b_LoadFromDBDoesNotBlockOtherThreads(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	st := NewSawtoothTrigger(time.Hour, 700000, 400000)
	st.SetLoadFunc(func(key string) (string, bool) {
		if strings.Contains(key, "los26b-thread-a") {
			started <- struct{}{}
			<-release
		}
		return `{"tokens":123456,"msg_count":7}`, true
	})

	loaded := make(chan int, 1)
	go func() { loaded <- st.GetLastTokens("los26b-thread-a") }()

	// Wait until the loader is actually inside loadFn.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the loader never started")
	}

	other := make(chan int, 1)
	go func() { other <- st.GetLastTokens("los26b-thread-b") }()
	select {
	case <-other:
	case <-time.After(500 * time.Millisecond):
		close(release)
		t.Fatal("GetLastTokens for an unrelated thread blocked while another thread's state was loading from the daemon")
	}

	close(release)
	select {
	case got := <-loaded:
		if got != 123456 {
			t.Fatalf("GetLastTokens = %d after the load finished, want 123456", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the load itself never completed")
	}
}

// --- K4: a single oversized message must not define the kept window ----------

// TestLos26b_OversizedToolResultDoesNotPinTheKeptWindow pins the fix for the
// collapse floor: one tool_result larger than the whole content budget used to
// satisfy the floor on its own inside the keepRecent window. The kept tail then
// never got below the budget, so every following request re-triggered the stub
// cycle without ever reaching the floor.
func TestLos26b_OversizedToolResultDoesNotPinTheKeptWindow(t *testing.T) {
	const (
		n          = 20
		keepRecent = 10
		floor      = 50000
	)
	msgs := make([]any, n)
	msgs[0] = map[string]any{"role": "user", "content": "start"}
	for i := 1; i < n; i++ {
		msgs[i] = map[string]any{"role": "user", "content": "filler"}
	}
	// The oversized result sits inside the keepRecent window (indices 10..19):
	// messages[15] is the tool_use, messages[16] its 400k-char result.
	msgs[15] = map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_los26b", "name": "Bash",
				"input": map[string]any{"command": "true"}},
		},
	}
	msgs[16] = map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_los26b",
				"content": strings.Repeat("x", 400000)},
		},
	}

	estimate := func(text string) int { return len(text) / 4 }
	cutoff := CalcCollapseCutoff(msgs, keepRecent, floor, estimate)
	if cutoff <= 0 {
		t.Fatalf("expected a cutoff, got %d", cutoff)
	}

	keptTokens := 0
	for i := cutoff; i < n; i++ {
		msg, ok := msgs[i].(map[string]any)
		if !ok {
			keptTokens += perMessageOverhead
			continue
		}
		keptTokens += estimateMessageContentTokens(msg, estimate)
	}
	if keptTokens >= floor {
		t.Fatalf("kept tail = %d tokens at cutoff=%d (budget %d); the oversized tool_result at index 16 alone keeps the payload above the budget and the collapse never reaches the floor", keptTokens, cutoff, floor)
	}

	first, _ := msgs[cutoff].(map[string]any)
	if first["role"] == "user" && hasToolResultContent(first["content"]) {
		t.Fatalf("cutoff=%d lands on an orphan tool_result whose tool_use is collapsed", cutoff)
	}
}

// --- K5: per-thread state must be evictable ---------------------------------

// TestLos26b_SawtoothStateIsPruned pins the fix for the unbounded per-thread
// maps (estimateFactor, pendingEstimate and the token/time maps). The thread id
// is client-controlled, so a client that keeps inventing ids grows the process
// memory without any upper bound.
func TestLos26b_SawtoothStateIsPruned(t *testing.T) {
	st := NewSawtoothTrigger(time.Hour, 700000, 400000)

	st.SetSentEstimate("los26b-old", 1000)
	st.UpdateAfterResponse("los26b-old", 100000, 20)
	st.SetSentEstimate("los26b-new", 1000)
	st.UpdateAfterResponse("los26b-new", 200000, 30)

	// Age the old thread beyond the TTL (same package: direct state access).
	st.mu.Lock()
	st.lastTouch["los26b-old"] = time.Now().Add(-2 * time.Hour)
	st.mu.Unlock()

	if got := st.Prune(time.Hour); got != 1 {
		t.Fatalf("Prune = %d, want 1 evicted thread", got)
	}

	st.mu.RLock()
	_, hasTokens := st.lastTotalTokens["los26b-old"]
	_, hasFactor := st.estimateFactor["los26b-old"]
	_, hasPending := st.pendingEstimate["los26b-old"]
	_, hasTouch := st.lastTouch["los26b-old"]
	newTokens := st.lastTotalTokens["los26b-new"]
	st.mu.RUnlock()

	if hasTokens || hasFactor || hasPending || hasTouch {
		t.Fatalf("state of the idle thread survived the prune (tokens=%v factor=%v pending=%v touch=%v)", hasTokens, hasFactor, hasPending, hasTouch)
	}
	if newTokens != 200000 {
		t.Fatalf("state of the active thread = %d tokens after the prune, want 200000", newTokens)
	}
}

// TestLos26b_PruneReachesThreadsWithoutPersistedState covers the hole the cold
// review found: loadFromDB registered a thread in loadedFromDB but only wrote
// lastTouch when the load actually returned state, so a thread whose load found
// nothing was invisible to Prune and stayed forever.
func TestLos26b_PruneReachesThreadsWithoutPersistedState(t *testing.T) {
	st := NewSawtoothTrigger(time.Hour, 700000, 400000)
	// No loader at all: the thread is registered as "load attempted, nothing found".
	st.GetLastTokens("los26b-empty")

	st.mu.Lock()
	_, hasTouch := st.lastTouch["los26b-empty"]
	st.lastTouch["los26b-empty"] = time.Now().Add(-2 * time.Hour)
	st.mu.Unlock()
	if !hasTouch {
		t.Fatal("a thread whose state load found nothing was never registered for eviction")
	}

	if got := st.Prune(time.Hour); got != 1 {
		t.Fatalf("Prune = %d, want 1 (the empty thread)", got)
	}
	st.mu.RLock()
	_, stillLoaded := st.loadedFromDB["los26b-empty"]
	st.mu.RUnlock()
	if stillLoaded {
		t.Fatal("loadedFromDB entry of an idle thread survived the prune")
	}
}

// --- Cold-review counter-cases ----------------------------------------------

// TestLos26b_ClaudeCodeSubagentMetadataStillDetected pins the marker the cold
// review asked for: a Claude Code Agent-tool subagent declares itself in
// metadata.user_id, which the stream attribution already trusts.
func TestLos26b_ClaudeCodeSubagentMetadataStillDetected(t *testing.T) {
	req := los26bMainReqNoThinking()
	req["metadata"] = map[string]any{
		"user_id": `{"device_id":"d","account_uuid":"a","session_id":"s","agent_type":"subagent"}`,
	}
	if !isSubagentRequest(http.Header{}, buildLargeMessages(3), req) {
		t.Fatal("metadata.user_id agent_type=subagent no longer identifies a subagent")
	}

	// A main session carries the same metadata shape without the agent type.
	main := los26bMainReqNoThinking()
	main["metadata"] = map[string]any{
		"user_id": `{"device_id":"d","account_uuid":"a","session_id":"s"}`,
	}
	if isSubagentRequest(http.Header{}, buildLargeMessages(3), main) {
		t.Fatal("main-session metadata without agent_type was classified as subagent")
	}
}

// TestLos26b_ErrorResponseLeavesSawtoothUntouched guards the widened usage gate:
// an upstream error body carries no usage and must not write trigger state.
func TestLos26b_ErrorResponseLeavesSawtoothUntouched(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"try later"}}`))
	}))
	defer backend.Close()

	s := los26aHandlerServer(t, backend.URL)
	s.cfg.SawtoothEnabled = true

	body := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},"messages":[` +
		`{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-opencode-session-id", "ses_los26b_error")
	s.handleMessages(httptest.NewRecorder(), req)

	if got := s.sawtoothTrigger.GetLastTokens("opencode:ses_los26b_error"); got != 0 {
		t.Fatalf("lastTotalTokens = %d after an error response, want 0", got)
	}
}

// TestLos26b_ConcurrentLoadsHitTheDaemonOnce pins the wait branch of loadFromDB:
// two callers for the same thread share one daemon round trip.
func TestLos26b_ConcurrentLoadsHitTheDaemonOnce(t *testing.T) {
	var calls int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	st := NewSawtoothTrigger(time.Hour, 700000, 400000)
	st.SetLoadFunc(func(key string) (string, bool) {
		atomic.AddInt32(&calls, 1)
		started <- struct{}{}
		<-release
		return `{"tokens":111,"msg_count":3}`, true
	})

	results := make(chan int, 2)
	go func() { results <- st.GetLastTokens("los26b-shared") }()
	<-started
	go func() { results <- st.GetLastTokens("los26b-shared") }()
	close(release)

	for i := 0; i < 2; i++ {
		select {
		case got := <-results:
			if got != 111 {
				t.Fatalf("GetLastTokens = %d, want 111", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a caller of the shared load never returned")
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("loadFn called %d times for one thread, want 1", n)
	}
}

// TestLos26b_ResponseDuringLoadWins pins the freshness rule: a response that
// lands while the old state is still loading must not be overwritten by it.
func TestLos26b_ResponseDuringLoadWins(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	st := NewSawtoothTrigger(time.Hour, 700000, 400000)
	st.SetLoadFunc(func(key string) (string, bool) {
		started <- struct{}{}
		<-release
		return `{"tokens":111,"msg_count":3}`, true
	})

	loading := make(chan int, 1)
	go func() { loading <- st.GetLastTokens("los26b-racing") }()
	<-started
	st.UpdateAfterResponse("los26b-racing", 999, 5)
	close(release)
	<-loading

	if got := st.GetLastTokens("los26b-racing"); got != 999 {
		t.Fatalf("lastTotalTokens = %d, want the fresher 999 from the response that landed during the load", got)
	}
}

// TestLos26b_OversizedCurrentTurnIsProtected pins the second half of K4 after
// the consequence review: a recent user prompt survives the oversized rule.
// The rule may collapse the huge tool_use between prompt and tool_result, but
// not the instruction that started the turn.
func TestLos26b_OversizedCurrentTurnIsProtected(t *testing.T) {
	msgs := make([]any, 10)
	msgs[0] = map[string]any{"role": "user", "content": "start"}
	for i := 1; i < 7; i++ {
		msgs[i] = map[string]any{"role": "user", "content": "filler"}
	}
	const promptIdx = 7
	msgs[promptIdx] = map[string]any{"role": "user", "content": "do the thing"}
	msgs[8] = map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_los26b_pair", "name": "Bash",
				"input": map[string]any{"command": strings.Repeat("x", 400000)}},
		},
	}
	msgs[9] = map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_los26b_pair", "content": "ok"},
		},
	}

	estimate := func(text string) int { return len(text) / 4 }
	// keepRecent=5 keeps the prompt inside the recent window, so it is the
	// current turn and must not be part of the collapsed prefix.
	cutoff := CalcCollapseCutoff(msgs, 5, 50000, estimate)
	if cutoff <= 0 {
		t.Fatalf("expected a cutoff, got %d", cutoff)
	}
	if cutoff > promptIdx {
		t.Fatalf("cutoff = %d collapses the current turn's instruction at index %d", cutoff, promptIdx)
	}
	first, _ := msgs[cutoff].(map[string]any)
	if first["role"] == "user" && hasToolResultContent(first["content"]) {
		t.Fatalf("cutoff=%d lands on an orphan tool_result", cutoff)
	}
}

// TestLos26b_OversizedRuleReachesAgenticToolLoop pins the consequence-review
// finding that the protection above made the rule dead code in an agentic
// session: there the only plain user message is the original instruction near
// the start, and every later user message is a tool_result. A huge tool_result
// inside the recent window must still be collapsible.
func TestLos26b_OversizedRuleReachesAgenticToolLoop(t *testing.T) {
	const (
		n          = 20
		keepRecent = 10
		floor      = 50000
	)
	msgs := make([]any, n)
	msgs[0] = map[string]any{"role": "user", "content": "start"}
	msgs[1] = map[string]any{"role": "user", "content": "the instruction"}
	for i := 2; i < n; i++ {
		if i%2 == 0 {
			msgs[i] = map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", i), "name": "Bash",
						"input": map[string]any{"command": "true"}},
				},
			}
		} else {
			msgs[i] = map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("toolu_%d", i-1), "content": "ok"},
				},
			}
		}
	}
	// The oversized result sits inside the recent window (indices 10..19).
	msgs[15] = map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_14",
				"content": strings.Repeat("x", 400000)},
		},
	}

	estimate := func(text string) int { return len(text) / 4 }
	cutoff := CalcCollapseCutoff(msgs, keepRecent, floor, estimate)
	if cutoff <= 15 {
		t.Fatalf("cutoff = %d keeps the 100k-token tool_result at index 15; the collapse never reaches the %d-token budget in an agentic session", cutoff, floor)
	}

	keptTokens := 0
	for i := cutoff; i < n; i++ {
		msg, ok := msgs[i].(map[string]any)
		if !ok {
			keptTokens += perMessageOverhead
			continue
		}
		keptTokens += estimateMessageContentTokens(msg, estimate)
	}
	if keptTokens >= floor {
		t.Fatalf("kept tail = %d tokens at cutoff=%d, want below the %d budget", keptTokens, cutoff, floor)
	}
}

// TestLos26b_LegacyAgentTypeHeaderIsSubagent pins the review finding that the
// legacy fork's agent-type header must keep identifying a child, even though it
// arrives together with the child's own session header.
func TestLos26b_LegacyAgentTypeHeaderIsSubagent(t *testing.T) {
	msgs := buildLargeMessages(4)
	req := map[string]any{"model": "claude-opus-4-6"}
	h := http.Header{}
	h.Set("X-Opencode-Agent-Type", "subagent")
	h.Set("x-opencode-session-id", "ses_los26b_legacy_child")
	if !isSubagentRequest(h, msgs, req) {
		t.Fatal("legacy agent-type header no longer identifies a subagent")
	}
}

// TestLos26b_PauseTriggerSurvivesColdLoad pins the consequence-review finding
// that Prune dropped lastRequestTime while persistedState had no time field:
// after eviction (or a proxy restart) a resuming thread kept its token count
// but lost the idle gap, so the cold-cache pause trigger never fired.
func TestLos26b_PauseTriggerSurvivesColdLoad(t *testing.T) {
	var saved string
	st := NewSawtoothTrigger(30*time.Minute, 200000, 100000)
	st.SetPersistFunc(func(key, value string) { saved = value })
	st.UpdateAfterResponse("los26b-pause", 150000, 20)
	if !strings.Contains(saved, `"ts":`) {
		t.Fatalf("persisted state carries no response time, so a cold load cannot see the idle gap: %s", saved)
	}

	old := fmt.Sprintf(`{"tokens":150000,"msg_count":20,"ts":%d}`, time.Now().Add(-2*time.Hour).Unix())
	cold := NewSawtoothTrigger(30*time.Minute, 200000, 100000)
	cold.SetLoadFunc(func(string) (string, bool) { return old, true })
	if got := cold.GetLastTokens("los26b-pause"); got != 150000 {
		t.Fatalf("GetLastTokens = %d after the cold load, want 150000", got)
	}
	if got := cold.ShouldTrigger("los26b-pause", 0, 200000); got != TriggerPause {
		t.Fatalf("ShouldTrigger = %q after a cold load 2h after the last response, want %q (the pause signal was lost)", got, TriggerPause)
	}
}

// TestLos26b_FutureTimestampIsRejected guards the newly trusted ts field:
// proxy_state is writable through the daemon socket, and a timestamp in the
// future would make the pause trigger impossible for that thread.
func TestLos26b_FutureTimestampIsRejected(t *testing.T) {
	future := fmt.Sprintf(`{"tokens":150000,"msg_count":20,"ts":%d}`, time.Now().Add(72*time.Hour).Unix())
	st := NewSawtoothTrigger(30*time.Minute, 200000, 100000)
	st.SetLoadFunc(func(string) (string, bool) { return future, true })
	if got := st.GetLastTokens("los26b-future"); got != 150000 {
		t.Fatalf("GetLastTokens = %d, want 150000 (only the timestamp is untrusted)", got)
	}

	st.mu.RLock()
	last, hasTime := st.lastRequestTime["los26b-future"]
	st.mu.RUnlock()
	if hasTime && time.Since(last) < 0 {
		t.Fatalf("a future timestamp from proxy_state was adopted (lastRequestTime=%v), which silences the pause trigger", last)
	}
}

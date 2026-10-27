package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// YM-331 (3): the frozen per-message store is keyed by the thread id. For a
// client without a session header that key is derived from message content and
// repeats across sessions, so per-session content frozen under it is replayed
// into a session it never belonged to. The channel block and the plan nudge are
// gated on ThreadIsSession; the rules reminder, the associative context, the
// plan checkpoint, the docs hint and the loop warning were not.
func TestLos56dFrozenSessionContentRequiresASessionKey(t *testing.T) {
	run := func(t *testing.T, threadIsSession bool) *TimestampMeta {
		t.Helper()
		s := los26aHandlerServer(t, "http://127.0.0.1:1")
		s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true, PlanCheckpoint: true, RulesReminder: true, AssocContext: true}
		los33DaemonSocket(t, s, func(method string, params map[string]any) string {
			switch method {
			case "get_plan":
				return `{"exists":true,"plan":"LOS56D-PLAN","status":"active"}`
			case "get_docs_hint":
				return `{"docs_hint":"LOS56D-DOCS"}`
			case "get_rules_block":
				return `{"content":"LOS56D-RULES","exists":true}`
			case "hybrid_search":
				return `{"results":[{"id":"9101","content":"LOS56D-ASSOC","score":80,"origin_tool":"user","project":""}]}`
			default:
				return "null"
			}
		})

		threadID := "derived-hash"
		if threadIsSession {
			threadID = "ses_los56d_frozen"
		}
		ctx := &openAIRequestContext{ThreadID: threadID, RawMsgCount: 3, ThreadIsSession: threadIsSession, UserQuery: "next step"}

		// Fresh message maps per pass: a message that already carries the meta
		// prefix is skipped, and the second pass has to build the entry.
		req1 := map[string]any{"model": "gpt-5.4", "messages": []any{
			map[string]any{"role": "user", "content": "start"},
			map[string]any{"role": "assistant", "content": "on it"},
			map[string]any{"role": "user", "content": "next step"},
		}}
		// First pass only sets the token baselines of the interval gates.
		s.annotateOpenAIMessageMetadata(req1, ctx, req1["messages"].([]any), "gpt-5.4", 1)

		req2 := map[string]any{"model": "gpt-5.4", "messages": []any{
			map[string]any{"role": "user", "content": "start"},
			map[string]any{"role": "assistant", "content": "on it"},
			map[string]any{"role": "user", "content": "next step"},
		}}
		// Second pass crosses the interval, so every checkpoint is due.
		s.annotateOpenAIMessageMetadata(req2, ctx, req2["messages"].([]any), "gpt-5.4", 200000)

		entry, ok := s.timestampStore.Get(threadID, 3)
		if !ok || entry == nil {
			t.Fatalf("no frozen entry for %s", threadID)
		}
		return entry
	}

	session := run(t, true)
	if session.DocsHint == "" {
		t.Fatal("control run: docs hint missing under a session key")
	}
	if session.PlanCheckpoint == "" {
		t.Fatal("control run: plan checkpoint missing under a session key")
	}

	derived := run(t, false)
	if derived.DocsHint != "" {
		t.Errorf("docs hint frozen under a derived thread key: %q", derived.DocsHint)
	}
	if derived.PlanCheckpoint != "" {
		t.Errorf("plan checkpoint frozen under a derived thread key: %q", derived.PlanCheckpoint)
	}
	if derived.Rules != "" {
		t.Errorf("rules reminder frozen under a derived thread key: %q", derived.Rules)
	}
	if derived.AssocContext != "" {
		t.Errorf("associative context frozen under a derived thread key: %q", derived.AssocContext)
	}
}

// YM-339 (3): the legacy cache path dereferenced cacheGate without a nil guard,
// while the OpenAI path checked it. A server without a gate panicked on the
// Anthropic path instead of simply staying uncached.
func TestLos56dCacheGateNilDoesNotPanic(t *testing.T) {
	backendURL, _ := los33Upstream(t)
	s := los26aHandlerServer(t, backendURL)
	s.cacheGate = nil
	s.cfg.SawtoothEnabled = false
	los33DaemonSocket(t, s, func(string, map[string]any) string { return "null" })

	rec := httptest.NewRecorder()
	s.handleMessages(rec, los33MessagesRequest(los33UserTurn("claude-opus-4-6"),
		map[string]string{"X-Claude-Code-Session-Id": "ses_los56d_nilgate"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

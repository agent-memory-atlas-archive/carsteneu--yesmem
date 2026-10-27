package proxy

import (
	"encoding/json"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// los57bDerivedTurn is a user turn WITHOUT any session header, so the thread id
// is derived from the message content (anthropicThread reports isSession=false).
// los57bLargeText pushes the request over the 10000-token interval, which is the
// condition both injectors wait for.
const los57bLargeText = "the quick brown fox jumps over the lazy dog. "

// los57bDerivedBody builds the request body for a derived-thread turn. The body
// is deterministic, so the derived thread id is the same for caller and handler.
func los57bDerivedBody(padding int) string {
	return `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},` +
		`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
		`"messages":[{"role":"user","content":"start the task"},` +
		`{"role":"assistant","content":"on it"},` +
		`{"role":"user","content":"` + strings.Repeat(los57bLargeText, padding) + `"}]}`
}

// YM-406 (PRX-P54, 1): the Anthropic path freezes LoopWarning, PlanCheckpoint and
// DocsHint into the TimestampStore, while the OpenAI path gates the same fields on
// ThreadIsSession. A derived thread id is the same string for every session of a
// client, so a frozen checkpoint or docs hint is replayed into a session that
// never asked for it.
func TestLos57bMessagesPathDoesNotFreezePerSessionContentOnDerivedThread(t *testing.T) {
	backendURL, forwarded := los33Upstream(t)
	s := los26aHandlerServer(t, backendURL)
	los33DaemonSocket(t, s, func(method string, params map[string]any) string {
		switch method {
		case "get_plan":
			return `{"plan":"LOS57B-PLAN-TEXT","status":"active","exists":true}`
		case "get_docs_hint":
			return `{"docs_hint":"LOS57B-DOCS-TEXT"}`
		default:
			return "null"
		}
	})
	logBuf := &lockedLogBuffer{}
	s.logger = log.New(logBuf, "", 0)
	// timestamps and the plan checkpoint on, everything else off: the two
	// injectors under test are the only ones that can fire here.
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true, PlanCheckpoint: true}

	body := los57bDerivedBody(900)
	req := los33MessagesRequest(body, nil)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parse test body: %v", err)
	}
	threadID, isSession := anthropicThread(parsed, req.Header)
	if isSession {
		t.Fatalf("test setup: expected a derived thread id, got a session id %q", threadID)
	}

	// Prime the interval counters so both injectors fire on this request.
	planCp.reset(threadID, 0)
	docsCp.reset(threadID, 0)

	rec := httptest.NewRecorder()
	s.handleMessages(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// The content must still reach the request (on the tail message, as the
	// OpenAI path does), otherwise the gate would silently drop the feature.
	sent := forwarded()
	for _, marker := range []string{"LOS57B-PLAN-TEXT", "LOS57B-DOCS-TEXT"} {
		if !strings.Contains(sent, marker) {
			t.Errorf("%s did not reach the upstream request, body:\n%s", marker, sent)
		}
	}

	// ... but it must not be frozen into the per-message store under a key that
	// every session of this client shares.
	entry, ok := s.timestampStore.Get(threadID, 3)
	if !ok {
		t.Fatalf("no timestamp entry stored for the derived thread, journal:\n%s", logBuf.String())
	}
	if entry.PlanCheckpoint != "" {
		t.Errorf("plan checkpoint frozen under the derived thread id: %q", entry.PlanCheckpoint)
	}
	if entry.DocsHint != "" {
		t.Errorf("docs hint frozen under the derived thread id: %q", entry.DocsHint)
	}
}

// YM-406 (PRX-P54, 1), read side: the write gate stops NEW frozen content under a
// derived thread id, but entries that an earlier build froze there are persisted
// as timestamps:<derived id> and survive a restart. The OpenAI path closes that
// with ScrubPerSessionContent on load; the Anthropic path re-rendered them into
// the next session that landed on the same key.
func TestLos57bMessagesPathScrubsFrozenContentOnDerivedThread(t *testing.T) {
	backendURL, forwarded := los33Upstream(t)
	s := los26aHandlerServer(t, backendURL)
	los33DaemonSocket(t, s, func(method string, params map[string]any) string { return "null" })
	logBuf := &lockedLogBuffer{}
	s.logger = log.New(logBuf, "", 0)
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true}

	body := los57bDerivedBody(1)
	req := los33MessagesRequest(body, nil)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parse test body: %v", err)
	}
	threadID, isSession := anthropicThread(parsed, req.Header)
	if isSession {
		t.Fatalf("test setup: expected a derived thread id, got a session id %q", threadID)
	}

	// What a build without the write gate left behind for msg:1 of this key.
	s.timestampStore.Store(threadID, 1, &TimestampMeta{
		Timestamp:      "Mo 2026-10-19 08:00:00",
		PlanCheckpoint: "LOS57B-FROZEN-LEAK",
	})

	rec := httptest.NewRecorder()
	s.handleMessages(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if sent := forwarded(); strings.Contains(sent, "LOS57B-FROZEN-LEAK") {
		t.Errorf("stored session content was replayed into this session, body:\n%s", sent)
	}
}

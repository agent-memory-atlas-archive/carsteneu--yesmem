package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// YM-349(6): the OpenAI path guards skill tracking with ThreadIsSession, the
// Anthropic path did not. Without any session header, anthropicThreadID falls
// back to a hash derived from the message content: the same key for every
// session of one client build. Tracking skills under that key replays one
// session's skill content into the next and runs a daemon RPC per detected
// pair. The guard is the header: no header, no session, no tracking.
func TestLos56c_AnthropicSkillTrackingRequiresRealSession(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	defer backend.Close()

	s := los26aHandlerServer(t, backend.URL)
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true, SkillEval: false}

	body := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},` +
		`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
		`"messages":[` +
		`{"role":"user","content":"start the task"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Skill","input":{"skill":"yesloop"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"skill loaded"}]},` +
		`{"role":"user","content":"continue"}]}`

	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	derived := DeriveThreadID(parsed)

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if names := s.skillTracker.activeSkills(derived); len(names) != 0 {
		t.Errorf("skills tracked under the content-derived key %q: %v", derived, names)
	}
}

// The header path keeps tracking: this is the same expectation as
// TestLos27_AnthropicPathTracksSkillActivationsWithGateOff, restated here so a
// guard that is too broad fails on its own fixture.
func TestLos56c_AnthropicSkillTrackingKeepsWorkingForSessions(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	defer backend.Close()

	s := los26aHandlerServer(t, backend.URL)
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true, SkillEval: false}

	const sessionID = "ses_los56c_guard"
	body := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},` +
		`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
		`"messages":[` +
		`{"role":"user","content":"start the task"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Skill","input":{"skill":"yesloop"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"skill loaded"}]},` +
		`{"role":"user","content":"continue"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !s.skillTracker.isActive(sessionID, "yesloop") {
		t.Error("session request lost its skill tracking")
	}
}

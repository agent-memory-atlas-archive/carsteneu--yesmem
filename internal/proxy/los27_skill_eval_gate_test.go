package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// YM-113: the Anthropic path injected the [skill-eval] block unconditionally,
// while the OpenAI-parity path gates the same block behind
// isFeatureEnabled(..., "skill_eval"). A model configured with
// skill_eval: false therefore still received the block on the Anthropic wire.
// YM-113 follow-up: gating the injection must not gate the tracker. The tracker
// is the only source for re-injecting skill content after a context collapse
// (buildSkillBlocksForThread reads skillTracker), so a model with
// skill_eval: false would otherwise lose skill content permanently.
func TestLos27_AnthropicPathTracksSkillActivationsWithGateOff(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	defer backend.Close()

	s := los26aHandlerServer(t, backend.URL)
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true, SkillEval: false}

	const sessionID = "ses_los27_tracker"
	body := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},` +
		`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
		`"messages":[` +
		`{"role":"user","content":"start the task"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Skill","input":{"skill":"test-driven-development"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"skill loaded"}]},` +
		`{"role":"user","content":"continue"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !s.skillTracker.isActive(sessionID, "test-driven-development") {
		t.Error("skill activation was not tracked while the skill-eval gate was off")
	}
}

func TestLos27_AnthropicPathRespectsSkillEvalGate(t *testing.T) {
	tests := []struct {
		name        string
		skillEval   bool
		wantBlockOn bool
	}{
		{name: "gate off suppresses the block", skillEval: false, wantBlockOn: false},
		{name: "gate on injects the block", skillEval: true, wantBlockOn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu       sync.Mutex
				upstream string
			)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				upstream = string(raw)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":1}}`))
			}))
			defer backend.Close()

			s := los26aHandlerServer(t, backend.URL)
			s.cfg.FeatureDefaults = &config.FeatureGates{
				Timestamps: true,
				SkillEval:  tt.skillEval,
			}

			body := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},` +
				`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
				`"messages":[{"role":"user","content":"start the task"},` +
				`{"role":"assistant","content":"on it"},` +
				`{"role":"user","content":"implement the i18n fix"}]}`
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			req.Header.Set("X-Claude-Code-Session-Id", "ses_los27_skill_eval")
			rec := httptest.NewRecorder()

			s.handleMessages(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}

			mu.Lock()
			defer mu.Unlock()
			if upstream == "" {
				t.Fatal("backend received no body")
			}
			hasBlock := strings.Contains(upstream, SkillEvalStartMarker)
			if hasBlock != tt.wantBlockOn {
				t.Errorf("skill-eval block present = %v, want %v\nupstream body:\n%s", hasBlock, tt.wantBlockOn, upstream)
			}
		})
	}
}

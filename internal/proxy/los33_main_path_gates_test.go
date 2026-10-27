package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// los33GateDaemon answers the daemon calls the four gated features make.
func los33GateDaemon(t *testing.T, s *Server) {
	t.Helper()
	los33DaemonSocket(t, s, func(method string, params map[string]any) string {
		switch method {
		case "generate_briefing":
			return `{"text":"LOS33-BRIEFING","code_map":""}`
		case "hybrid_search":
			return `{"results":[{"id":"9001","content":"LOS33-ASSOC","score":80,"origin_tool":"user","project":""}]}`
		case "get_rules_block":
			return `{"content":"LOS33-RULES","exists":true}`
		default:
			return "null"
		}
	})
}

// YM-137: model_features gates (briefing, rules_reminder, think_reminder,
// assoc_context) were only consulted in the OpenAI parity path. On the
// Anthropic path they had no effect, so a model configured with a feature off
// still received it there.
func TestLos33MessagesPathRespectsFeatureGates(t *testing.T) {
	cases := []struct {
		name   string
		gate   string
		marker string
	}{
		{name: "briefing", gate: "briefing", marker: "LOS33-BRIEFING"},
		{name: "assoc_context", gate: "assoc_context", marker: "LOS33-ASSOC"},
		{name: "think_reminder", gate: "think_reminder", marker: "Use hybrid_search() to check your memory first"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, gateOn := range []bool{false, true} {
				backendURL, forwarded := los33Upstream(t)
				s := los26aHandlerServer(t, backendURL)
				s.cfg.FeatureDefaults = los33Gates(tc.gate, gateOn)
				los33GateDaemon(t, s)

				session := "ses_los33_gate_" + tc.name
				if gateOn {
					session += "_on"
				} else {
					session += "_off"
				}
				req := los33MessagesRequest(los33UserTurn("claude-opus-4-6"),
					map[string]string{"X-Claude-Code-Session-Id": session})
				rec := httptest.NewRecorder()

				s.handleMessages(rec, req)

				if rec.Code != 200 {
					t.Fatalf("gate %s=%v: expected 200, got %d", tc.gate, gateOn, rec.Code)
				}
				body := forwarded()
				got := strings.Contains(body, tc.marker)
				if got != gateOn {
					t.Errorf("feature %q present = %v with the gate %v\nbody:\n%s",
						tc.name, got, gateOn, body)
				}
			}
		})
	}
}

// los33Gates returns FeatureDefaults with timestamps on and the named feature
// at the requested state, so every other gated injection stays out of the way.
func los33Gates(feature string, on bool) *config.FeatureGates {
	g := &config.FeatureGates{Timestamps: true}
	switch feature {
	case "briefing":
		g.Briefing = on
	case "assoc_context":
		g.AssocContext = on
	case "think_reminder":
		g.ThinkReminder = on
	case "rules_reminder":
		g.RulesReminder = on
	}
	return g
}

// YM-137 (rules_reminder): the rules block is re-injected after a token
// interval, so the gate has to be honoured at both call sites. This drives the
// real interval instead of calling the injector directly.
func TestLos33MessagesPathRespectsRulesReminderGate(t *testing.T) {
	big := strings.Repeat("x", 4*rulesInjectionInterval)

	for _, gateOn := range []bool{false, true} {
		backendURL, forwarded := los33Upstream(t)
		s := los26aHandlerServer(t, backendURL)
		s.cfg.FeatureDefaults = los33Gates("rules_reminder", gateOn)
		los33GateDaemon(t, s)

		session := "ses_los33_rules_" + map[bool]string{true: "on", false: "off"}[gateOn]

		// First request sets the token baseline; the second crosses the interval.
		first := los33MessagesRequest(los33UserTurn("claude-opus-4-6"),
			map[string]string{"X-Claude-Code-Session-Id": session})
		s.handleMessages(httptest.NewRecorder(), first)

		secondBody := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},` +
			`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
			`"messages":[{"role":"user","content":"` + big + `"},` +
			`{"role":"assistant","content":"on it"},` +
			`{"role":"user","content":"continue"}]}`
		second := los33MessagesRequest(secondBody,
			map[string]string{"X-Claude-Code-Session-Id": session})
		s.handleMessages(httptest.NewRecorder(), second)

		body := forwarded()
		if got := strings.Contains(body, "LOS33-RULES"); got != gateOn {
			t.Errorf("rules reminder present = %v with rules_reminder=%v\nbody:\n%s", got, gateOn, body)
		}
	}
}

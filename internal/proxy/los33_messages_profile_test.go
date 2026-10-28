package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/models"
)

// YM-136: handleMessages resolved the prompt flags for ProfileClaude
// unconditionally. The wire format is not the client: opencode speaks
// /v1/messages too, and it must not receive the Claude-only directive blocks.
func TestLos33MessagesPathUsesClientProfile(t *testing.T) {
	backendURL, forwarded := recordingUpstream(t)

	s := los26aHandlerServer(t, backendURL)
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true}
	s.cfg.Prompt = &config.ProxyConfig{
		SharedPrompt: &config.PromptFlags{OutputDiscipline: true},
		ClaudePrompt: &config.PromptFlags{ToolPrefs: true},
	}

	cases := []struct {
		name         string
		headers      map[string]string
		wantToolPref bool
	}{
		{
			name:         "claude code client keeps the claude profile",
			headers:      map[string]string{"X-Claude-Code-Session-Id": "ses_los33_profile_cc"},
			wantToolPref: true,
		},
		{
			name:         "opencode client does not get claude-only blocks",
			headers:      map[string]string{"x-opencode-session": "ses_los33_profile_oc"},
			wantToolPref: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := messagesRequest(userTurnBody("claude-opus-4-6"), tc.headers)
			rec := httptest.NewRecorder()

			s.handleMessages(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			body := forwarded()
			if body == "" {
				t.Fatal("backend received no body")
			}
			if got := strings.Contains(body, "[yesmem-tool-prefs]"); got != tc.wantToolPref {
				t.Errorf("claude-only tool-prefs block present = %v, want %v", got, tc.wantToolPref)
			}
			if !strings.Contains(body, "[yesmem-output-discipline]") {
				t.Error("a shared_prompt flag must reach every profile")
			}
		})
	}
}

// YM-136 wiring: Run builds the server from proxy.Config, so the prompt layers
// must travel inside that value. A separate server field that nobody assigns is
// what left the profile selection dead in production before this los.
func TestLos33ServerHonoursPromptLayersFromConfig(t *testing.T) {
	cfg := Config{Prompt: &config.ProxyConfig{
		SharedPrompt:   &config.PromptFlags{Beweislast: true},
		OpencodePrompt: &config.PromptFlags{ClarifyFirst: true},
	}}
	s := &Server{cfg: cfg}

	if !s.getPromptFlags(models.ProfileOpencode).ClarifyFirst {
		t.Error("opencode layer did not reach getPromptFlags through proxy.Config")
	}
	if s.getPromptFlags(models.ProfileClaude).ClarifyFirst {
		t.Error("opencode layer leaked into the claude profile")
	}
	if !s.getPromptFlags(models.ProfileClaude).Beweislast {
		t.Error("shared layer did not reach the claude profile")
	}
}

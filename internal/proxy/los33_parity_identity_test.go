package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// YM-144: prepareOpenAIRequestContext labelled every client "opencode", so a
// codex request carried an opencode thread key although codex is a different
// client with a different profile.
func TestLos33ParityPathKeysCodexThreadsAsCodex(t *testing.T) {
	s := los26aHandlerServer(t, "http://127.0.0.1:1")
	req := map[string]any{
		"model":    "glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}

	codexCtx := s.prepareOpenAIRequestContext(req, 1, "", "", "codex-tui/0.132.0 (linux mint)", nil)
	if !strings.HasPrefix(codexCtx.ThreadID, "codex:") {
		t.Errorf("codex thread id = %q, want the codex: prefix", codexCtx.ThreadID)
	}
	if codexCtx.ThreadIsSession {
		t.Error("a derived codex thread id must not be treated as a session")
	}

	ocCtx := s.prepareOpenAIRequestContext(req, 1, "", "ses_los33_parity_oc", "opencode/1.2.3", nil)
	if ocCtx.ThreadID != "opencode:ses_los33_parity_oc" {
		t.Errorf("opencode thread id = %q, want opencode:ses_los33_parity_oc", ocCtx.ThreadID)
	}
}

// YM-144: the parity pipeline requested models.ProfileCodex for every client,
// so an opencode_prompt layer could never take effect there.
func TestLos33ParityPathUsesTheClientProfile(t *testing.T) {
	s := los26aHandlerServer(t, "http://127.0.0.1:1")
	s.cfg.FeatureDefaults = &config.FeatureGates{}
	s.cfg.Prompt = &config.ProxyConfig{
		CodexPrompt:    &config.PromptFlags{ScopeDiscipline: true},
		OpencodePrompt: &config.PromptFlags{ClarifyFirst: true},
	}

	parityBody := func(t *testing.T, userAgent, ocSession string) string {
		t.Helper()
		req := map[string]any{
			"model": "glm-5.2",
			"messages": []any{
				map[string]any{"role": "user", "content": "start"},
				map[string]any{"role": "assistant", "content": "on it"},
				map[string]any{"role": "user", "content": "check the profile routing"},
			},
		}
		ctx := s.prepareOpenAIRequestContext(req, 1, "", ocSession, userAgent, nil)
		s.runOpenAIParityPipeline(req, &ctx)

		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		return string(raw)
	}

	codexBody := parityBody(t, "codex-tui/0.132.0 (linux mint)", "")
	if !strings.Contains(codexBody, "[yesmem-scope-discipline]") {
		t.Error("codex_prompt layer did not reach the codex request")
	}
	if strings.Contains(codexBody, "[yesmem-clarify-first]") {
		t.Error("codex request received the opencode_prompt layer")
	}

	ocBody := parityBody(t, "opencode/1.2.3", "ses_los33_profile_layer")
	if !strings.Contains(ocBody, "[yesmem-clarify-first]") {
		t.Error("opencode request did not receive the opencode_prompt layer")
	}
	if strings.Contains(ocBody, "[yesmem-scope-discipline]") {
		t.Error("opencode request received the codex_prompt layer")
	}
}

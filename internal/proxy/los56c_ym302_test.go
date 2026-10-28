package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"gopkg.in/yaml.v3"
)

// los56cPatternSuggestServer wires a handler server whose daemon socket records
// the RPC methods, so a side effect of the REPL pattern recording is visible.
func los56cPatternSuggestServer(t *testing.T, prompt *config.ProxyConfig) (*Server, chan string) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	t.Cleanup(backend.Close)

	s := los26aHandlerServer(t, backend.URL)
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true}
	s.cfg.Prompt = prompt
	seen := make(chan string, 128)
	s.cfg.DataDir = startFakeDaemon(t, func(method string, _ map[string]any) string {
		select {
		case seen <- method:
		default:
		}
		return ""
	})
	s.daemonReady.Store(true)
	return s, seen
}

// los56cTurnSequenceBody carries one closed assistant turn with a tool_use, so
// computeTurnHashFromMessages has something to hash.
const los56cTurnSequenceBody = `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},` +
	`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
	`"messages":[` +
	`{"role":"user","content":"run the tests"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"go test ./..."}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]},` +
	`{"role":"user","content":"continue"}]}`

func los56cWaitForMethod(t *testing.T, seen chan string, method string, want bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	var all []string
	for {
		select {
		case got := <-seen:
			all = append(all, got)
			if got == method {
				if !want {
					t.Fatalf("%s was called although the profile switched it off (all: %v)", method, all)
				}
				return
			}
		case <-deadline:
			if want {
				t.Fatalf("%s was never called (all: %v)", method, all)
			}
			return
		}
	}
}

// YM-302(1): both recorders read the flat ProxyConfig field
// (s.cfg.PromptPatternSuggest) instead of the resolved profile layer, so a
// prompt_profile setting had no reader at all. The layer is the current
// mechanism; the flat field is the deprecated compatibility default.
func TestLos56c_PatternSuggestFollowsTheResolvedProfile(t *testing.T) {
	prompt := &config.ProxyConfig{
		SharedPrompt: &config.PromptFlags{PatternSuggest: true},
	}
	s, seen := los56cPatternSuggestServer(t, prompt)
	// The flat field stays off: only the profile layer is on.
	s.cfg.PromptPatternSuggest = false

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(los56cTurnSequenceBody))
	req.Header.Set("x-opencode-session", "ses_los56c_pattern")
	rec := httptest.NewRecorder()
	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	los56cWaitForMethod(t, seen, "record_turn_sequence", true)
}

func TestLos56c_PatternSuggestProfileCanSwitchItOff(t *testing.T) {
	var prompt config.ProxyConfig
	// Decoded from YAML so the layer records which keys it names: only then can
	// an opencode layer switch off what the shared layer turned on.
	if err := yaml.Unmarshal([]byte("shared_prompt:\n  prompt_pattern_suggest: true\nopencode_prompt:\n  prompt_pattern_suggest: false\n"), &prompt); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	s, seen := los56cPatternSuggestServer(t, &prompt)
	// The deprecated flat field claims true and must not overrule the layer.
	s.cfg.PromptPatternSuggest = true

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(los56cTurnSequenceBody))
	req.Header.Set("x-opencode-session", "ses_los56c_pattern_off")
	rec := httptest.NewRecorder()
	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	los56cWaitForMethod(t, seen, "record_turn_sequence", false)
}

// YM-302(2): a model block with min_chars 0 (or one the user never set) made
// the reminder unconditional even when feature_defaults named a minimum: the
// resolver stopped at the first matching block instead of falling back.
func TestLos56c_ThinkReminderMinCharsFallsBackToFeatureDefaults(t *testing.T) {
	cfg := &Config{
		ModelFeatures: map[string]*config.FeatureGates{
			"claude": {ThinkReminder: true},
		},
		FeatureDefaults: &config.FeatureGates{ThinkReminderMinChars: 10},
	}
	if got := resolveThinkReminderMinChars(cfg, "claude-opus-4-6"); got != 10 {
		t.Errorf("per-model block without a minimum: got %d, want the feature_defaults value 10", got)
	}

	cfg.ModelFeatures["claude"].ThinkReminderMinChars = 25
	if got := resolveThinkReminderMinChars(cfg, "claude-opus-4-6"); got != 25 {
		t.Errorf("per-model minimum ignored: got %d, want 25", got)
	}

	empty := &Config{}
	if got := resolveThinkReminderMinChars(empty, "unlisted-model"); got != 0 {
		t.Errorf("no config at all: got %d, want 0", got)
	}
}

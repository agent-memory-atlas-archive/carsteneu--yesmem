package proxy

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestRoutingFallbackWarnsOncePerModel(t *testing.T) {
	s := &Server{
		autoProviderTargets: map[string]string{
			"big-pickle": "https://opencode.ai/zen",
		},
		cfg: Config{
			OpenAITargetURL: "https://api.openai.com",
			TargetURL:       "https://api.anthropic.com",
		},
	}

	// Unknown model → fallback resolution records a one-shot warning.
	s.resolveOpenAITarget("unknown-model")
	if !s.routingWarned["unknown-model"] {
		t.Fatalf("expected fallback warning for unknown-model, got %v", s.routingWarned)
	}

	// Second resolution of the same model must not duplicate the entry.
	s.resolveOpenAITarget("unknown-model")
	if len(s.routingWarned) != 1 {
		t.Fatalf("flood guard should keep exactly one entry, got %d: %v", len(s.routingWarned), s.routingWarned)
	}

	// A different unknown model gets its own entry.
	s.resolveOpenAITarget("other-model")
	if !s.routingWarned["other-model"] {
		t.Fatalf("expected fallback warning for other-model, got %v", s.routingWarned)
	}
}

func TestRoutingFallbackNoWarningForCoveredModel(t *testing.T) {
	s := &Server{
		cfg: Config{
			ProviderTargets: map[string]string{
				"glm-5.2": "https://api.z.ai/api/coding/paas/v4",
			},
			OpenAITargetURL: "https://api.openai.com",
		},
	}

	s.resolveOpenAITarget("glm-5.2")
	if len(s.routingWarned) != 0 {
		t.Fatalf("covered model must not produce a fallback warning, got %v", s.routingWarned)
	}
}

func TestRoutingFallbackWarningLogLine(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	s := &Server{
		cfg: Config{
			OpenAITargetURL: "https://api.openai.com",
		},
	}

	s.resolveOpenAITarget("mystery-model")
	logged := buf.String()
	if !strings.Contains(logged, "[routing]") {
		t.Fatalf("expected [routing] warning in log, got %q", logged)
	}
	if !strings.Contains(logged, "mystery-model") {
		t.Fatalf("expected model name in log line, got %q", logged)
	}
	if !strings.Contains(logged, "provider_targets") {
		t.Fatalf("expected hint to add provider_targets entry, got %q", logged)
	}
}

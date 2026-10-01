package extraction

import (
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

func TestLooksLikeStateSnapshot(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"tool count snapshot", "YesMem has 70 MCP tools registered", true},
		{"health snapshot", "Proxy is in degraded state with 6 restarts", true},
		{"version snapshot", "reply_model was deepseek-v4-pro as of 2026-06-27", true},
		{"hit ratio", "Global prefix cache hit-ratio was 93.8% as of 20.08.", true},
		{"insight with number", "`pytest | tail` masks the exit code — failing suites appear green; gates must use pipefail.", false},
		{"preference", "User prefers German responses", false},
		{"no digits", "The proxy caches system blocks", false},
	}
	for _, tc := range cases {
		if got := LooksLikeStateSnapshot(tc.content); got != tc.want {
			t.Errorf("%s: LooksLikeStateSnapshot = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyVolatileBatch(t *testing.T) {
	client := &mockLLMClient{
		completeFunc: func(system, userMsg string) (string, error) {
			return `[{"id": 1, "verdict": "volatile", "confidence": 0.95, "reason": "live-derivable tool count"},
{"id": 2, "verdict": "durable", "confidence": 0.9, "reason": "durable workflow rule"}]`, nil
		},
		model: "haiku",
	}

	batch := []models.Learning{
		{ID: 1, Content: "YesMem has 70 MCP tools registered"},
		{ID: 2, Content: "Piping pytest into tail hides failures"},
	}

	verdicts, err := ClassifyVolatileBatch(client, batch)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("expected 2 verdicts, got %d", len(verdicts))
	}
	if verdicts[0].Verdict != "volatile" || verdicts[0].Confidence != 0.95 {
		t.Errorf("unexpected verdict 0: %+v", verdicts[0])
	}
	if verdicts[1].Verdict != "durable" {
		t.Errorf("unexpected verdict 1: %+v", verdicts[1])
	}
}

func TestClassifyVolatileBatch_Empty(t *testing.T) {
	verdicts, err := ClassifyVolatileBatch(&mockLLMClient{}, nil)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if verdicts != nil {
		t.Errorf("expected nil verdicts for empty batch, got %+v", verdicts)
	}
}

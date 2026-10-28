package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
)

// The daemon meters real usage with the configured price table, not with the
// builtin fallback in extraction: an unknown provider model would otherwise be
// billed at sonnet rates and hit the daily budget far too early.
func TestDecorateDaemonLLMClient_MetersAtConfiguredPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "ok"}},
			},
			"usage": map[string]int{"input_tokens": 1_000_000, "output_tokens": 1_000_000},
		})
	}))
	defer srv.Close()

	ac := config.Default()
	if ac.SecretsSanitization.Enabled {
		t.Fatal("this test expects the undecorated-by-redaction path")
	}
	ac.Pricing = map[string]config.ModelPricing{
		"unknown-provider-model": {Input: 0.28, Output: 1.12},
	}

	raw, err := extraction.NewLLMClient("openai_compatible", "key", "unknown-provider-model", "", srv.URL+"/v1")
	if err != nil {
		t.Fatalf("build raw client: %v", err)
	}
	tracker := extraction.NewBudgetTracker(100.0)
	client := decorateDaemonLLMClient(raw, ac, llmClientOpts{
		model:   "unknown-provider-model",
		tracker: tracker,
	})

	if _, err := client.Complete("sys", "msg"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	spent, calls := tracker.SpentToday()
	if calls != 1 {
		t.Fatalf("metered %d calls, want 1", calls)
	}
	if want := 0.28 + 1.12; spent < want-1e-9 || spent > want+1e-9 {
		t.Errorf("spent $%.6f, want $%.6f: the configured prices must be applied, not the builtin 3.0/15.0 fallback", spent, want)
	}
}

package extraction

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// usageServer answers chat completions with a fixed token count so a test can
// predict the exact cost of one call.
func usageServer(t *testing.T, inputTokens, outputTokens int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "ok"}},
			},
			"usage": map[string]int{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
			},
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// realCost mirrors the arithmetic a tracker applies to measured token counts.
func realCost(inputTokens, outputTokens int) float64 {
	inPerM, outPerM := PricingForModel("gpt-5.2")
	return float64(inputTokens)/1_000_000*inPerM + float64(outputTokens)/1_000_000*outPerM
}

// Every daemon LLM path shares one package-wide OnUsage hook, so the real token
// counts of a quality call used to be booked onto the extraction tracker, and
// the quality tracker then booked its own char estimate on top: the same call
// paid twice, once in the wrong bucket. The measured usage must reach the
// tracker of the client that made the call, and only that one.
func TestBudgetClient_RealUsageStaysOnItsOwnTracker(t *testing.T) {
	const in, out = 100_000, 10_000
	srv := usageServer(t, in, out)

	own := NewBudgetTracker(10.0)   // the bucket this client meters against
	other := NewBudgetTracker(10.0) // stands in for the other bucket

	prev := OnUsage
	var globalCalls int
	OnUsage = func(model string, inputTokens, outputTokens int) {
		globalCalls++
		other.TrackTokens(inputTokens, outputTokens, 1.75, 14.0)
	}
	defer func() { OnUsage = prev }()

	client := NewOpenAIClient("k", "gpt-5.2", srv.URL+"/v1", "openai_compatible")
	bc := NewBudgetClient(client, own)

	if _, err := bc.Complete("sys", "msg"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if globalCalls != 0 {
		t.Errorf("measured usage reached the package-wide hook %d time(s), want 0", globalCalls)
	}
	if _, calls := other.SpentToday(); calls != 0 {
		t.Errorf("foreign tracker booked %d call(s), want 0", calls)
	}
	spent, calls := own.SpentToday()
	if calls != 1 {
		t.Fatalf("own tracker booked %d calls, want 1", calls)
	}
	if want := realCost(in, out); spent < want-1e-9 || spent > want+1e-9 {
		t.Errorf("own tracker spent $%.6f, want $%.6f (measured usage, no estimate on top)", spent, want)
	}
}

// The old double-count guard was one int32 per tracker: concurrent calls through
// one client consumed each other's signal, so one call was booked twice and
// another not at all. The decision belongs to the call, not to the tracker.
func TestBudgetClient_ConcurrentCallsBookEachCallOnce(t *testing.T) {
	const in, out = 20_000, 2_000
	const calls = 20
	srv := usageServer(t, in, out)

	tracker := NewBudgetTracker(1000.0)
	client := NewOpenAIClient("k", "gpt-5.2", srv.URL+"/v1", "openai_compatible")
	bc := NewBudgetClient(client, tracker)

	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := bc.Complete("sys", "msg"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent complete: %v", err)
	}

	spent, booked := tracker.SpentToday()
	if booked != calls {
		t.Fatalf("tracker booked %d calls, want %d", booked, calls)
	}
	want := realCost(in, out) * calls
	if spent < want-1e-6 || spent > want+1e-6 {
		t.Errorf("tracker spent $%.6f, want $%.6f (%d measured calls, no estimates)", spent, want, calls)
	}
}

// failingLLM reports usage and then fails, the way the opencode path does when
// the call consumed tokens but produced no text. The spend happened.
type failingLLM struct{ model string }

func (f *failingLLM) Name() string  { return "failing" }
func (f *failingLLM) Model() string { return f.model }
func (f *failingLLM) fail(opts []CallOption) (string, error) {
	if sink := applyOpts(opts).usageSink; sink != nil {
		sink(100, 10)
	}
	return "", fmt.Errorf("no text events")
}
func (f *failingLLM) Complete(system, userMsg string, opts ...CallOption) (string, error) {
	return f.fail(opts)
}
func (f *failingLLM) CompleteJSON(system, userMsg string, schema map[string]any, opts ...CallOption) (string, error) {
	return f.fail(opts)
}

// A failed call that already reported its token counts must still be booked: the
// provider charged for it, and the previous code booked on OnUsage before the
// caller ever saw the error.
func TestBudgetClient_BooksReportedUsageOnFailure(t *testing.T) {
	tracker := NewBudgetTracker(10.0)
	bc := NewBudgetClient(&failingLLM{model: "gpt-5.2"}, tracker)

	if _, err := bc.Complete("sys", "msg"); err == nil {
		t.Fatal("expected the inner error to reach the caller")
	}

	spent, calls := tracker.SpentToday()
	if calls != 1 {
		t.Fatalf("booked %d calls, want 1 booking for the reported usage", calls)
	}
	if want := realCost(100, 10); spent < want-1e-12 || spent > want+1e-12 {
		t.Errorf("spent $%.12f, want $%.12f", spent, want)
	}
}

// The built-in price table does not know every provider model. A caller with a
// config-aware table must be able to hand its prices to the client, otherwise
// measured usage is priced far above what the provider bills.
func TestBudgetClient_SetPricingAppliesToMeasuredUsage(t *testing.T) {
	srv := usageServer(t, 1_000_000, 1_000_000)
	tracker := NewBudgetTracker(1000.0)
	client := NewOpenAIClient("k", "deepseek-v4-pro", srv.URL+"/v1", "openai_compatible")
	bc := NewBudgetClient(client, tracker)
	bc.SetPricing(0.28, 1.12) // config table value, not the builtin sonnet fallback

	if _, err := bc.Complete("sys", "msg"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	spent, _ := tracker.SpentToday()
	if want := 0.28 + 1.12; spent < want-1e-9 || spent > want+1e-9 {
		t.Errorf("spent $%.6f, want $%.6f from the injected prices", spent, want)
	}
}

// estimateCost keeps the estimate arithmetic visible for the fallback assertion.
func estimateCost(system, userMsg, result string) float64 {
	inPerM, outPerM := PricingForModel("gpt-5.2")
	inputTokens := (len(system) + len(userMsg)) / 4
	outputTokens := len(result) / 4
	return float64(inputTokens)/1_000_000*inPerM + float64(outputTokens)/1_000_000*outPerM
}

// multiStepLLM reports several steps for one call, the way the opencode transport
// does: one step_finish per tool roundtrip.
type multiStepLLM struct {
	model string
	steps [][2]int
}

func (m *multiStepLLM) Name() string  { return "multistep" }
func (m *multiStepLLM) Model() string { return m.model }
func (m *multiStepLLM) report(opts []CallOption) {
	sink := applyOpts(opts).usageSink
	for _, s := range m.steps {
		if sink != nil {
			sink(s[0], s[1])
		}
	}
}
func (m *multiStepLLM) Complete(system, userMsg string, opts ...CallOption) (string, error) {
	m.report(opts)
	return "ok", nil
}
func (m *multiStepLLM) CompleteJSON(system, userMsg string, schema map[string]any, opts ...CallOption) (string, error) {
	m.report(opts)
	return `{"ok":true}`, nil
}

// A call that reports several steps must be booked with the sum of them. Booking
// only the last step understates the spend silently, which is the failure the
// per-call sink was introduced to end.
func TestBudgetClient_AccumulatesMultipleStepsOfOneCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		jsonRun bool
	}{
		{name: "complete", jsonRun: false},
		{name: "completeJSON", jsonRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewBudgetTracker(10.0)
			inner := &multiStepLLM{model: "gpt-5.2", steps: [][2]int{{100, 10}, {200, 20}}}
			bc := NewBudgetClient(inner, tracker)

			var err error
			if tc.jsonRun {
				_, err = bc.CompleteJSON("sys", "msg", nil)
			} else {
				_, err = bc.Complete("sys", "msg")
			}
			if err != nil {
				t.Fatalf("complete: %v", err)
			}

			spent, calls := tracker.SpentToday()
			if calls != 1 {
				t.Fatalf("booked %d calls, want 1 booking per call", calls)
			}
			if want := realCost(300, 30); spent < want-1e-12 || spent > want+1e-12 {
				t.Errorf("spent $%.12f, want $%.12f (sum of both steps)", spent, want)
			}
		})
	}
}

// Without a transport that reports usage the char estimate stays the fallback;
// the estimate must not be skipped just because a sink exists.
func TestBudgetClient_FallsBackToEstimateWithoutReportedUsage(t *testing.T) {
	tracker := NewBudgetTracker(10.0)
	bc := NewBudgetClient(&mockLLM{model: "gpt-5.2"}, tracker)

	if _, err := bc.Complete("sys", "msg"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	spent, calls := tracker.SpentToday()
	if calls != 1 {
		t.Fatalf("booked %d calls, want 1", calls)
	}
	want := estimateCost("sys", "msg", "ok")
	if spent < want-1e-12 || spent > want+1e-12 {
		t.Errorf("spent $%.12f, want the char estimate $%.12f", spent, want)
	}
	if fmt.Sprintf("%.6f", spent) == "0.000000" {
		t.Error("estimate booked nothing")
	}
}

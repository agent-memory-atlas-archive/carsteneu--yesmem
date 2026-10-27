package extraction

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// ── YM-301 (Los 09c): Token-Meldung der Transports ──────────────────────────

// los09cRoundTrip lets a test answer the Anthropic client, whose endpoint is a
// package constant, without leaving the process.
type los09cRoundTrip func(*http.Request) (*http.Response, error)

func (f los09cRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func los09cJSONResponse(t *testing.T, body string) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A usage block full of zeros is what several OpenAI-compatible gateways send
// when they do not meter a request. Reporting it marks the call as MEASURED in
// BudgetClient.book, so the whole call is booked at $0 and disappears from the
// daily budget, while withholding it falls back to the char estimate. Only the
// CLI transports carried that guard.
func TestLos09cZeroUsageReportIsNotBooked(t *testing.T) {
	const systemChars = 40_000 // ~10k estimated input tokens
	const userChars = 4_000
	system := strings.Repeat("s", systemChars)
	user := strings.Repeat("u", userChars)

	tests := []struct {
		name  string
		build func(*testing.T) LLMClient
	}{
		{
			name: "anthropic messages",
			build: func(t *testing.T) LLMClient {
				c := NewClient("k", "claude-sonnet-4-6")
				c.httpClient = &http.Client{Transport: los09cRoundTrip(func(*http.Request) (*http.Response, error) {
					return los09cJSONResponse(t, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":0,"output_tokens":0}}`), nil
				})}
				return c
			},
		},
		{
			name: "openai chat completions",
			build: func(t *testing.T) LLMClient {
				return NewOpenAIClient("k", "gpt-5.2", usageServer(t, 0, 0).URL+"/v1", "openai_compatible")
			},
		},
		{
			name: "openai responses",
			build: func(t *testing.T) LLMClient {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{
						"output_text": "ok",
						"usage":       map[string]int{"input_tokens": 0, "output_tokens": 0},
					})
				}))
				t.Cleanup(srv.Close)
				return NewOpenAIClient("k", "gpt-5.2", srv.URL, "openai")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewBudgetTracker(100.0)
			bc := NewBudgetClient(tt.build(t), tracker)

			if _, err := bc.Complete(system, user); err != nil {
				t.Fatalf("complete: %v", err)
			}

			spent, calls := tracker.SpentToday()
			if calls != 1 {
				t.Fatalf("booked %d calls, want exactly 1", calls)
			}
			if spent <= 0 {
				t.Errorf("spent $%f: the 0/0 usage report replaced the char estimate and dropped the call from the budget", spent)
			}
		})
	}
}

// The claude CLI envelope carries the cache read count as well, and the same
// ratio applies: a run whose whole prompt was a cache hit must not be handed to
// the char estimate, which would price the full prompt at the input rate.
func TestLos09cExtractAndReportUsageCountsCacheReads(t *testing.T) {
	const cacheRead = 9_000
	payload := `{"type":"result","result":"ok","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":9000}}`
	cli := NewCLIClient("claude", "test-model", models.SourceAgentClaude)

	var gotIn, gotOut, calls int
	got := cli.extractAndReportUsage(payload, func(in, out int) {
		calls++
		gotIn, gotOut = in, out
	})
	if got != "ok" {
		t.Errorf("result = %q, want \"ok\"", got)
	}
	if calls != 1 {
		t.Fatalf("usage reported %d time(s), want 1", calls)
	}
	if want := cacheRead / 10; gotIn != want {
		t.Errorf("billable input = %d, want %d (cache read %d at a tenth of the input rate)", gotIn, want, cacheRead)
	}
	if gotOut != 0 {
		t.Errorf("output = %d, want 0", gotOut)
	}
}

// opencode emits one step_finish per tool roundtrip. A step whose whole prompt
// was a cache hit reports input=0 and cache.read>0; reporting nothing makes the
// call fall back to the char estimate, which prices the full prompt at the input
// rate. Cache reads are billed at a tenth of that.
func TestLos09cParseOpencodeOutputReportsCacheReadOnlyStep(t *testing.T) {
	const cacheRead = 5_000
	output := strings.Join([]string{
		`{"type":"step_finish","part":{"tokens":{"input":0,"output":0,"cache":{"read":5000,"write":0}}}}`,
		`{"type":"text","part":{"text":"ok"}}`,
	}, "\n")

	var gotIn, gotOut int
	reports := 0
	sink := func(in, out int) {
		reports++
		gotIn += in
		gotOut += out
	}

	if _, err := parseOpencodeOutput(output, "gpt-5.2", sink); err != nil {
		t.Fatalf("parseOpencodeOutput: %v", err)
	}
	if reports == 0 {
		t.Fatal("a cache-read-only step reported nothing, so the call falls back to the char estimate")
	}
	if want := cacheRead / 10; gotIn != want {
		t.Errorf("reported input = %d, want %d (cache read %d at a tenth of the input rate)", gotIn, want, cacheRead)
	}
	if gotOut != 0 {
		t.Errorf("reported output = %d, want 0", gotOut)
	}
}

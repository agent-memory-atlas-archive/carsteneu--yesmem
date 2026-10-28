package extraction

import (
	"net/http"
	"testing"
)

// ── YM-313 (Los 56f): Anthropic-Client meldet die Cache-Felder ──────────────

// The client marks its system block with cache_control, so the API answers with
// cache_creation_input_tokens and cache_read_input_tokens next to input_tokens,
// and input_tokens excludes both. Dropping the fields booked every cached call
// at the price of its uncached remainder, so a session whose system prompt is
// almost entirely cached disappeared from the budget. The counts go through
// billableInput, the same formula the CLI transport uses.
func TestLos56fAnthropicCacheTokensAreBooked(t *testing.T) {
	const body = `{"content":[{"type":"text","text":"ok"}],` +
		`"usage":{"input_tokens":100,"output_tokens":20,` +
		`"cache_creation_input_tokens":800,"cache_read_input_tokens":5000}}`

	c := NewClient("k", "claude-sonnet-4-6")
	c.httpClient = &http.Client{Transport: los09cRoundTrip(func(*http.Request) (*http.Response, error) {
		return los09cJSONResponse(t, body), nil
	})}

	prev := OnUsage
	t.Cleanup(func() { OnUsage = prev })
	var gotIn, gotOut int
	var calls int
	OnUsage = func(model string, inputTokens, outputTokens int) {
		calls++
		gotIn, gotOut = inputTokens, outputTokens
	}

	if _, err := c.Complete("system", "user"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnUsage called %d times, want 1", calls)
	}
	if gotOut != 20 {
		t.Errorf("output tokens = %d, want 20", gotOut)
	}
	// 100 plain + 800 cache writes at the input rate + 5000 cache reads at a
	// tenth (rounded up), expressed in input tokens so the two-counter sink books
	// what the metering chain bills.
	const want = 100 + 800 + 5000/10
	if gotIn != want {
		t.Errorf("input tokens = %d, want %d (plain 100, cache-write 800, cache-read 5000)", gotIn, want)
	}
}

// A response without the cache fields (an older model or a gateway that does
// not cache) must keep reporting exactly its input_tokens.
func TestLos56fAnthropicUsageWithoutCacheFields(t *testing.T) {
	const body = `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":120,"output_tokens":7}}`

	c := NewClient("k", "claude-sonnet-4-6")
	c.httpClient = &http.Client{Transport: los09cRoundTrip(func(*http.Request) (*http.Response, error) {
		return los09cJSONResponse(t, body), nil
	})}

	prev := OnUsage
	t.Cleanup(func() { OnUsage = prev })
	var gotIn int
	OnUsage = func(model string, inputTokens, outputTokens int) { gotIn = inputTokens }

	if _, err := c.Complete("system", "user"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotIn != 120 {
		t.Errorf("input tokens = %d, want 120", gotIn)
	}
}

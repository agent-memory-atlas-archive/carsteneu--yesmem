package extraction

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// YM-412: the OpenAI path booked only the uncached-looking totals, so cache
// hits inside prompt_tokens were paid at the uncached input rate. The Anthropic
// path has priced them since YM-313 (billableInput).

func captureUsage(t *testing.T) (func() (int, int), func()) {
	t.Helper()
	old := OnUsage
	var in, out int
	OnUsage = func(_ string, inputTokens, outputTokens int) {
		in, out = inputTokens, outputTokens
	}
	return func() (int, int) { return in, out }, func() { OnUsage = old }
}

func chatServer(t *testing.T, usage string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":` + usage + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIChatBooksNestedCachedTokens(t *testing.T) {
	srv := chatServer(t, `{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":800}}`)
	got, restore := captureUsage(t)
	defer restore()

	c := NewOpenAIClient("sk", "gpt-5-mini", srv.URL, "openai_compatible")
	if _, err := c.Complete("sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	in, out := got()
	// 200 uncached input + 800 cache reads at a tenth (cacheReadInputEquivalent).
	if in != 280 {
		t.Fatalf("booked input = %d, want 280 (200 uncached + 800 cache reads at their rate)", in)
	}
	if out != 10 {
		t.Fatalf("booked output = %d, want 10", out)
	}
}

func TestOpenAIChatBooksDeepSeekCacheFields(t *testing.T) {
	srv := chatServer(t, `{"prompt_tokens":1000,"completion_tokens":10,"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100}`)
	got, restore := captureUsage(t)
	defer restore()

	c := NewOpenAIClient("sk", "deepseek-v4", srv.URL, "openai_compatible")
	if _, err := c.Complete("sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	in, _ := got()
	// 100 cache misses at full rate + 900 cache hits at a tenth.
	if in != 190 {
		t.Fatalf("booked input = %d, want 190 (100 misses + 900 hits at their rate)", in)
	}
}

func TestOpenAIChatBooksCacheWrites(t *testing.T) {
	srv := chatServer(t, `{"prompt_tokens":1000,"completion_tokens":10,"cache_creation_input_tokens":100,"prompt_tokens_details":{"cached_tokens":800}}`)
	got, restore := captureUsage(t)
	defer restore()

	c := NewOpenAIClient("sk", "gateway-model", srv.URL, "openai_compatible")
	if _, err := c.Complete("sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	in, _ := got()
	// 100 uncached + 100 cache writes at full rate + 800 reads at a tenth.
	if in != 280 {
		t.Fatalf("booked input = %d, want 280 (100 uncached + 100 writes + 800 reads at their rate)", in)
	}
}

func TestOpenAIResponsesBooksCachedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output_text":"ok","usage":{"input_tokens":1000,"output_tokens":10,"input_tokens_details":{"cached_tokens":500}}}`))
	}))
	defer srv.Close()

	got, restore := captureUsage(t)
	defer restore()

	c := NewOpenAIClient("sk", "gpt-5-mini", srv.URL, "openai")
	if _, err := c.Complete("sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	in, _ := got()
	// 500 uncached input + 500 cache reads at a tenth.
	if in != 550 {
		t.Fatalf("booked input = %d, want 550 (500 uncached + 500 cache reads at their rate)", in)
	}
}

// Responses names inside a chat body: input_tokens is the total, so the nested
// cached count is a part of it and must be taken out before pricing.
func TestOpenAIChatBooksResponsesNamesWithCachedDetail(t *testing.T) {
	srv := chatServer(t, `{"input_tokens":1000,"output_tokens":10,"prompt_tokens_details":{"cached_tokens":800}}`)
	got, restore := captureUsage(t)
	defer restore()

	c := NewOpenAIClient("sk", "gateway-model", srv.URL, "openai_compatible")
	if _, err := c.Complete("sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	in, _ := got()
	if in != 280 {
		t.Fatalf("booked input = %d, want 280 (200 uncached + 800 cache reads at their rate)", in)
	}
}

// The Anthropic field names are exclusive counters: input_tokens does not
// contain them, so they are added, never subtracted.
func TestOpenAIChatBooksExclusiveAnthropicCounters(t *testing.T) {
	srv := chatServer(t, `{"input_tokens":1000,"output_tokens":10,"cache_read_input_tokens":800,"cache_creation_input_tokens":100}`)
	got, restore := captureUsage(t)
	defer restore()

	c := NewOpenAIClient("sk", "gateway-model", srv.URL, "openai_compatible")
	if _, err := c.Complete("sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	in, _ := got()
	// 1000 uncached + 100 cache writes at full rate + 800 reads at a tenth.
	if in != 1180 {
		t.Fatalf("booked input = %d, want 1180 (1000 + 100 writes + 800 reads at their rate)", in)
	}
}

// A miss counter is a component of the prompt total, so it may raise the booked
// amount but never lower it: a gateway reporting it in another unit must not be
// able to talk the budget down.
func TestOpenAIChatMissCounterCannotUnderbook(t *testing.T) {
	srv := chatServer(t, `{"prompt_tokens":500000,"completion_tokens":10,"prompt_cache_miss_tokens":1}`)
	got, restore := captureUsage(t)
	defer restore()

	c := NewOpenAIClient("sk", "gateway-model", srv.URL, "openai_compatible")
	if _, err := c.Complete("sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	in, _ := got()
	if in != 500000 {
		t.Fatalf("booked input = %d, want 500000 (the prompt total is the floor)", in)
	}
}

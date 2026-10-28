package extraction

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// YM-406 (PRX-P54, 3): the forwarding client of the proxy refuses redirects
// (http.ErrUseLastResponse, YM-339) because a redirect can carry the API key to
// a host the caller never configured. The extraction clients had no such rule:
// they send the same Authorization header, so a 3xx from the configured base URL
// would replay the credential wherever the Location points.
func TestLos57bOpenAIClientDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Int32
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"stolen"}]}]}`))
	}))
	defer victim.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, victim.URL+"/v1/responses", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := NewOpenAIClient("secret-key", "gpt-5.5", redirector.URL, "openai_compatible")
	_, err := c.Complete("system", "user")

	if got := reached.Load(); got != 0 {
		t.Errorf("the extraction client followed the redirect %d time(s) and carried the API key to the Location host", got)
	}
	// A 3xx must name itself. Without the explicit check the Location body lands
	// in json.Unmarshal and the caller sees "invalid character '<'".
	if err == nil {
		t.Fatal("a refused redirect produced no error")
	}
	if !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), victim.URL) {
		t.Errorf("redirect error is not self-explaining: %v", err)
	}
}

// The Anthropic client posts to a fixed endpoint, so the rule cannot be exercised
// with a test server. The guard is the client configuration itself.
func TestLos57bAnthropicExtractionClientRefusesRedirects(t *testing.T) {
	c := NewClient("secret-key", "claude-sonnet-4-6")
	if c.httpClient.CheckRedirect == nil {
		t.Fatal("extraction client has no CheckRedirect: a 3xx would be followed and would carry the API key")
	}
	req, err := http.NewRequest("POST", "https://example.invalid/v1/messages", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := c.httpClient.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
)

// echoLLMClient returns its prompt, which makes the redaction of the wrapper
// observable from the outside.
type echoLLMClient struct{}

func (echoLLMClient) Complete(system, userMsg string, opts ...extraction.CallOption) (string, error) {
	return userMsg, nil
}

func (echoLLMClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...extraction.CallOption) (string, error) {
	return userMsg, nil
}

func (echoLLMClient) Name() string  { return "echo" }
func (echoLLMClient) Model() string { return "echo" }

func TestRedacted_LeavesClientUntouchedWhenDisabled(t *testing.T) {
	var raw extraction.LLMClient = echoLLMClient{}
	if got := redacted(&config.Config{}, raw); got != raw {
		t.Fatal("secrets_sanitization disabled must return the client unchanged")
	}
}

func TestRedacted_WrapsAndRedactsWhenEnabled(t *testing.T) {
	cfg := &config.Config{SecretsSanitization: config.SecretsSanitizationConfig{Enabled: true}}
	wrapped := redacted(cfg, echoLLMClient{})
	if _, ok := wrapped.(*extraction.SanitizingClient); !ok {
		t.Fatalf("secrets_sanitization enabled must wrap the client, got %T", wrapped)
	}

	const prompt = `{"api_key": "abc123def456ghi789jkl012mno"}`
	out, err := wrapped.Complete("sys", prompt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "abc123def456ghi789jkl012mno") {
		t.Fatalf("maintenance client forwarded a secret: %q", out)
	}
	if !strings.Contains(out, "[REDACTED:generic_api_key]") {
		t.Fatalf("expected the marker, got %q", out)
	}
}

func TestParseLearningIDs(t *testing.T) {
	ids, err := parseLearningIDs(" 12, 34 ,,56 ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ids) != 3 || ids[0] != 12 || ids[1] != 34 || ids[2] != 56 {
		t.Errorf("ids = %v, want [12 34 56]", ids)
	}

	for _, raw := range []string{"", " , ", "12,abc"} {
		if _, err := parseLearningIDs(raw); err == nil {
			t.Errorf("parseLearningIDs(%q) must fail", raw)
		}
	}
}

func TestClipTextStripsControlChars(t *testing.T) {
	// A learning can carry escape sequences from ingested text; the listing must
	// not hand them to the terminal.
	if got := clipText("evil\x1b[2Jclear", 40); strings.ContainsRune(got, 0x1b) {
		t.Errorf("clipText left an escape sequence in %q", got)
	}
	if got := clipText(strings.Repeat("ä", 5), 3); got != "äää…" {
		t.Errorf("clipText = %q, want %q", got, "äää…")
	}
	if got := clipText("short", 10); got != "short" {
		t.Errorf("clipText must not change short content, got %q", got)
	}
}

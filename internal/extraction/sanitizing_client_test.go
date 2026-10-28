package extraction

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/sanitize"
)

type spyLLMClient struct {
	lastSystem  string
	lastUserMsg string
	response    string
	jsonResp    string
	name        string
	model       string
	errReturn   error
}

func (s *spyLLMClient) Complete(system, userMsg string, opts ...CallOption) (string, error) {
	s.lastSystem = system
	s.lastUserMsg = userMsg
	return s.response, s.errReturn
}

func (s *spyLLMClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...CallOption) (string, error) {
	s.lastSystem = system
	s.lastUserMsg = userMsg
	return s.jsonResp, s.errReturn
}

func (s *spyLLMClient) Name() string  { return s.name }
func (s *spyLLMClient) Model() string { return s.model }

const leakedKey = "sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestSanitizingClient_Complete_RedactsBothInputsBeforeSend(t *testing.T) {
	spy := &spyLLMClient{response: "ok"}
	wrapped := NewSanitizingClient(spy, sanitize.NewSecretRedactor(nil))
	_, err := wrapped.Complete("system has "+leakedKey, "user has "+leakedKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(spy.lastSystem, "sk-ant-api03") {
		t.Fatalf("inner client received unredacted system: %q", spy.lastSystem)
	}
	if strings.Contains(spy.lastUserMsg, "sk-ant-api03") {
		t.Fatalf("inner client received unredacted userMsg: %q", spy.lastUserMsg)
	}
}

func TestSanitizingClient_Complete_RedactsResponseAfterReceive(t *testing.T) {
	spy := &spyLLMClient{response: "leaked " + leakedKey}
	wrapped := NewSanitizingClient(spy, sanitize.NewSecretRedactor(nil))
	out, err := wrapped.Complete("hi", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "sk-ant-api03") {
		t.Fatalf("response not redacted: %q", out)
	}
	if !strings.Contains(out, "[REDACTED:anthropic_api_key]") {
		t.Fatalf("expected marker in response, got %q", out)
	}
}

func TestSanitizingClient_CompleteJSON_RedactsBothDirections(t *testing.T) {
	spy := &spyLLMClient{jsonResp: `{"key":"` + leakedKey + `"}`}
	wrapped := NewSanitizingClient(spy, sanitize.NewSecretRedactor(nil))
	out, err := wrapped.CompleteJSON("sys", "msg "+leakedKey, map[string]any{"type": "object"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(spy.lastUserMsg, "sk-ant-api03") {
		t.Fatalf("inner CompleteJSON received unredacted userMsg: %q", spy.lastUserMsg)
	}
	if strings.Contains(out, "sk-ant-api03") {
		t.Fatalf("CompleteJSON response not redacted: %q", out)
	}
	if !strings.Contains(out, "[REDACTED:anthropic_api_key]") {
		t.Fatalf("expected marker in CompleteJSON response, got %q", out)
	}
}

func TestSanitizingClient_PassesThroughNameAndModel(t *testing.T) {
	spy := &spyLLMClient{name: "mock-provider", model: "mock-1"}
	wrapped := NewSanitizingClient(spy, sanitize.NewSecretRedactor(nil))
	if wrapped.Name() != "mock-provider" {
		t.Errorf("Name passthrough broken: got %q", wrapped.Name())
	}
	if wrapped.Model() != "mock-1" {
		t.Errorf("Model passthrough broken: got %q", wrapped.Model())
	}
}

func TestSanitizingClient_Complete_ErrorPathSanitizesOutput(t *testing.T) {
	spy := &spyLLMClient{response: "leaked " + leakedKey, errReturn: errors.New("upstream down")}
	wrapped := NewSanitizingClient(spy, sanitize.NewSecretRedactor(nil))
	out, err := wrapped.Complete("sys", "user")
	if err == nil {
		t.Fatal("expected error from inner client")
	}
	if strings.Contains(out, leakedKey) {
		t.Fatalf("error path must sanitize output to prevent leaks via logs/persistence, got %q", out)
	}
	if !strings.Contains(out, "[REDACTED:anthropic_api_key]") {
		t.Fatalf("expected marker in error-path output, got %q", out)
	}
}

func TestSanitizingClient_CompleteJSON_ErrorPathSanitizesOutput(t *testing.T) {
	spy := &spyLLMClient{jsonResp: `{"key":"` + leakedKey + `"}`, errReturn: errors.New("upstream down")}
	wrapped := NewSanitizingClient(spy, sanitize.NewSecretRedactor(nil))
	out, err := wrapped.CompleteJSON("sys", "user", map[string]any{"type": "object"})
	if err == nil {
		t.Fatal("expected error from inner client")
	}
	if strings.Contains(out, leakedKey) {
		t.Fatalf("error path must sanitize output to prevent leaks via logs/persistence, got %q", out)
	}
	if !strings.Contains(out, "[REDACTED:anthropic_api_key]") {
		t.Fatalf("expected marker in error-path JSON output, got %q", out)
	}
}

func TestSanitizingClient_Complete_ErrorTextIsSanitized(t *testing.T) {
	spy := &spyLLMClient{errReturn: errors.New("cli exit 1: api_key=abc123def456ghi789jkl012mno")}
	wrapped := NewSanitizingClient(spy, sanitize.NewSecretRedactor(nil))

	_, err := wrapped.Complete("sys", "user")
	if err == nil {
		t.Fatal("expected error from inner client")
	}
	if strings.Contains(err.Error(), "abc123def456ghi789jkl012mno") {
		t.Fatalf("error text leaked a secret: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED:generic_api_key]") {
		t.Fatalf("expected the marker in the error text, got %v", err)
	}
}

func TestSanitizingClient_CompleteJSON_ErrorTextIsSanitized(t *testing.T) {
	spy := &spyLLMClient{errReturn: errors.New("cli stderr: " + leakedKey)}
	wrapped := NewSanitizingClient(spy, sanitize.NewSecretRedactor(nil))

	_, err := wrapped.CompleteJSON("sys", "user", map[string]any{"type": "object"})
	if err == nil {
		t.Fatal("expected error from inner client")
	}
	if strings.Contains(err.Error(), leakedKey) {
		t.Fatalf("error text leaked a secret: %v", err)
	}
}

func TestSanitizingClient_RedactedErrorKeepsChainWithoutRawText(t *testing.T) {
	sentinel := errors.New("budget exceeded")
	spy := &spyLLMClient{errReturn: fmt.Errorf("cli said api_key=abc123def456ghi789jkl012mno: %w", sentinel)}
	wrapped := NewSanitizingClient(spy, sanitize.NewSecretRedactor(nil))

	_, err := wrapped.Complete("sys", "user")
	if err == nil {
		t.Fatal("expected error from inner client")
	}
	if !errors.Is(err, sentinel) {
		t.Fatal("errors.Is must still reach the sentinel the inner error wraps")
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("errors.Unwrap must not hand out the unredacted error")
	}
	if strings.Contains(err.Error(), "abc123def456ghi789jkl012mno") {
		t.Fatalf("error text leaked a secret: %v", err)
	}
}

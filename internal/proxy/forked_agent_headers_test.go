package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Regression: fork requests keep the original body (incl. thinking and its
// beta-gated fields such as block_binding) but dropped the anthropic-beta
// header, so opencode forks were rejected with HTTP 400 "Extra inputs are not
// permitted" — the same bug as the cache keepalive ping.
func TestDoForkCall_AnthropicForwardsBetaHeader(t *testing.T) {
	var gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{}}`))
	}))
	defer srv.Close()

	s := &Server{httpClient: srv.Client()}
	orig := http.Header{}
	orig.Set("X-Api-Key", "sk-test")
	beta := "interleaved-thinking-2025-05-14,thinking-binding-controls-2026-08-01"
	orig.Set("anthropic-beta", beta)

	if _, err := s.doForkCall(srv.URL, "", orig, []byte(`{}`), false); err != nil {
		t.Fatalf("doForkCall: %v", err)
	}
	if gotBeta != beta {
		t.Errorf("fork must forward anthropic-beta %q, got %q", beta, gotBeta)
	}
}

func TestDoForkCall_AnthropicOmitsBetaWhenAbsent(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Anthropic-Beta"]
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{}}`))
	}))
	defer srv.Close()

	s := &Server{httpClient: srv.Client()}
	orig := http.Header{}
	orig.Set("X-Api-Key", "sk-test")

	if _, err := s.doForkCall(srv.URL, "", orig, []byte(`{}`), false); err != nil {
		t.Fatalf("doForkCall: %v", err)
	}
	if present {
		t.Error("fork must not send an empty anthropic-beta header")
	}
}

// Consequence review 5.3 finding 2: with Claude Code sessions now allowed to
// fork, the Anthropic fork call must keep the subscription credential in the
// Authorization header. Stripping "Bearer " into x-api-key failed with 401.
func TestDoForkCall_AnthropicKeepsBearerAuthorization(t *testing.T) {
	gotAuth := make(chan [2]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotAuth <- [2]string{r.Header.Get("Authorization"), r.Header.Get("x-api-key")}:
		default:
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{}}`))
	}))
	defer srv.Close()

	s := &Server{httpClient: srv.Client()}
	orig := http.Header{}
	orig.Set("Authorization", "Bearer sk-ant-oat01-abc")

	if _, err := s.doForkCall(srv.URL, "", orig, []byte(`{}`), false); err != nil {
		t.Fatalf("doForkCall: %v", err)
	}

	select {
	case h := <-gotAuth:
		if h[0] != "Bearer sk-ant-oat01-abc" {
			t.Errorf("fork must keep Authorization %q, got %q", "Bearer sk-ant-oat01-abc", h[0])
		}
		if h[1] != "" {
			t.Errorf("fork must not put an oauth token in x-api-key, got %q", h[1])
		}
	default:
		t.Fatal("no fork request reached the server")
	}
}

// The OpenAI-compatible fork path must not receive Anthropic beta headers.
func TestDoForkCall_OpenAIDoesNotSendBetaHeader(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Anthropic-Beta"]
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{}}`))
	}))
	defer srv.Close()

	s := &Server{httpClient: srv.Client()}
	orig := http.Header{}
	orig.Set("Authorization", "Bearer sk-test")
	orig.Set("anthropic-beta", "thinking-binding-controls-2026-08-01")

	if _, err := s.doForkCall(srv.URL, "", orig, []byte(`{}`), true); err != nil {
		t.Fatalf("doForkCall: %v", err)
	}
	if present {
		t.Error("OpenAI fork path must not send anthropic-beta")
	}
}

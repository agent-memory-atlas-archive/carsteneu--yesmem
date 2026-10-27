package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// los39cForkServer is the upstream the client's own request is routed to. It
// answers /v1/responses with a streamed completion and records every
// chat-completions request that reaches it (the fork).
type los39cForkServer struct {
	*httptest.Server
	forkAuth chan string
	forkBody chan string
}

func los39cNewForkServer(t *testing.T) *los39cForkServer {
	t.Helper()
	srv := &los39cForkServer{
		forkAuth: make(chan string, 1),
		forkBody: make(chan string, 1),
	}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":{"type":"text_delta","text":"ok"}}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"usage":{"input_tokens":50,"output_tokens":10,"input_tokens_details":{"cached_tokens":950}}}}`)
			flusher.Flush()
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			select {
			case srv.forkAuth <- r.Header.Get("Authorization"):
			default:
			}
			select {
			case srv.forkBody <- string(body):
			default:
			}
			w.Write([]byte(`{"choices":[{"message":{"content":"{\"learnings\":[]}"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// los39cFireFork sends one /v1/responses request through the proxy so the fork
// path runs. authorize controls whether the client brings its own credential.
func los39cFireFork(t *testing.T, s *Server, authorize bool) {
	t.Helper()
	oldDelay := openAIForkDelay
	openAIForkDelay = 0
	t.Cleanup(func() { openAIForkDelay = oldDelay })

	threadID := "opencode:ses_los39cfork"
	s.forkState.MarkCacheWarm(threadID)
	s.forkState.MarkCacheWarm(threadID)
	s.forkState.ForceNextFork(threadID)

	reqBody, _ := json.Marshal(map[string]any{
		"model":        "gpt-5.4",
		"instructions": "Be helpful",
		"input":        "Hello",
		"stream":       true,
	})
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if authorize {
		req.Header.Set("Authorization", "Bearer sk-test-openai")
	}
	req.Header.Set("x-opencode-session", "ses_los39cfork")
	w := httptest.NewRecorder()
	s.handleResponses(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("main request failed: status=%d body=%s", w.Code, w.Body.String())
	}
}

// YM-326 Befund 1: der Fork reichte den Client-Bearer auch an ein Ziel, das nicht
// das Ziel des Client-Requests war. Damit wandert der Schluessel eines Anbieters
// zum naechsten. Das Credential darf nur an die eigene Origin gehen.
func TestLos39c_ForkKeepsClientCredentialOnItsOwnOrigin(t *testing.T) {
	own := los39cNewForkServer(t)
	var foreignHits int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&foreignHits, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer foreign.Close()

	patched := make(chan struct{}, 1)
	s := &Server{
		cfg: Config{
			TargetURL:       own.URL,
			OpenAITargetURL: own.URL,
			// The fork model resolves to a different provider than the client's model.
			ProviderTargets: map[string]string{"glm": foreign.URL},
		},
		httpClient: own.Client(),
		logger:     log.New(io.Discard, "", 0),
		forkState:  NewForkState(0, 0, 3, 10),
	}
	s.forkConfigs = []ForkConfig{{
		Name:      "los39c_cross_origin",
		Model:     "glm-4.6",
		MaxTokens: 64,
		Prompt:    func(ForkContext) string { return "[FORK]" },
		ParseResult: func(ForkResponse, *Server) error {
			select {
			case patched <- struct{}{}:
			default:
			}
			return nil
		},
	}}

	los39cFireFork(t, s, true)

	time.Sleep(500 * time.Millisecond)
	if hits := atomic.LoadInt64(&foreignHits); hits != 0 {
		t.Errorf("fork called the foreign origin %d time(s): the client credential of one provider must not travel to another", hits)
	}
	select {
	case <-patched:
		t.Error("fork reported success although it had no credential for its target")
	default:
	}

	// A skipped fork must release the pending slot. ShouldFork marks the thread
	// pending; only RecordFork/RecordFailure clear it, and a pending thread never
	// forks again for the life of the process.
	if !s.forkState.ShouldFork("opencode:ses_los39cfork", 2_000_000, true, false) {
		t.Error("the skipped fork left the thread pending: extraction is dead for this session")
	}
}

// YM-326 Review-Befund I1: die Quell-Origin darf nicht aus dem geklonten Body
// abgeleitet werden. opencode sendet "providerID/modelID", der Body wird nach dem
// Routing auf den nackten Namen gestrippt, und die Ableitung zeigt dann auf einen
// anderen Anbieter mit demselben Modellnamen.
func TestLos39c_ForkCredentialUsesRecordedSourceTarget(t *testing.T) {
	s := &Server{
		cfg:       Config{ProviderTargets: map[string]string{"glm-4.6": "https://c.example"}},
		logger:    log.New(io.Discard, "", 0),
		forkState: NewForkState(0, 0, 3, 10),
	}
	body := []byte(`{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}`)
	auth := http.Header{"Authorization": {"Bearer sk-zai"}}

	// Control: the fork lands on the origin the client's request went to.
	same := ForkContext{OriginalBody: body, SourceFormat: "openai", SourceTarget: "https://a.example", AuthHeader: auth}
	if headers, _, usable := s.forkAuthFor(same, "https://a.example", true); headers == nil || !usable {
		t.Error("fork on the client's own origin must keep the client credential")
	}

	// The clone's model resolves to another provider; the recorded target decides.
	cross := ForkContext{OriginalBody: body, SourceFormat: "openai", SourceTarget: "https://a.example", AuthHeader: auth}
	if headers, key, usable := s.forkAuthFor(cross, "https://c.example", true); headers != nil || key != "" || usable {
		t.Errorf("fork got a credential for https://c.example although the client's request went to %q", cross.SourceTarget)
	}
}

// YM-326 Sicherheits-Review (Re-Review): ein geklonter Body behaelt beta-gesteuerte
// Felder, also muss anthropic-beta auf JEDEM Zweig mitgehen, auch auf dem
// Normalfall gleicher Origin.
func TestLos39c_ForkKeepsBetaHeaderOnSameOrigin(t *testing.T) {
	s := &Server{
		cfg:       Config{TargetURL: "https://api.anthropic.com", APIKey: "sk-ant-secret"},
		logger:    log.New(io.Discard, "", 0),
		forkState: NewForkState(0, 0, 3, 10),
	}
	auth := http.Header{}
	auth.Set("Authorization", "Bearer sk-ant-oat01-client")
	auth.Set("anthropic-beta", "thinking-block-binding-2026-01-01")

	ctx := ForkContext{
		OriginalBody: []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`),
		SourceFormat: "anthropic",
		SourceTarget: "https://api.anthropic.com",
		AuthHeader:   auth,
	}
	headers, _, usable := s.forkAuthFor(ctx, "https://api.anthropic.com", false)
	if !usable {
		t.Fatal("fork at the client's own origin must be usable")
	}
	if got := headers.Get("anthropic-beta"); got != "thinking-block-binding-2026-01-01" {
		t.Errorf("anthropic-beta = %q, want the client's beta header (the cloned body keeps beta-gated fields)", got)
	}
	if got := headers.Get("Authorization"); got != "Bearer sk-ant-oat01-client" {
		t.Errorf("Authorization = %q, want the client's credential", got)
	}
	if headers.Get("Cookie") != "" || headers.Get("X-Other") != "" {
		t.Errorf("fork must carry only credentials and protocol headers, got %v", headers)
	}
}

// YM-326 Sicherheits-Review (HIGH): der Anthropic-Key gehoert nur an den
// konfigurierten Upstream, nicht an einen anthropic-kompatiblen Drittanbieter,
// den provider_targets fuer dasselbe Modell benennt.
func TestLos39c_AnthropicKeyStaysAtItsOwnTarget(t *testing.T) {
	s := &Server{
		cfg:       Config{TargetURL: "https://api.anthropic.com", APIKey: "sk-ant-secret"},
		logger:    log.New(io.Discard, "", 0),
		forkState: NewForkState(0, 0, 3, 10),
	}
	body := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)

	// The client's request went to a third-party Anthropic-compatible provider.
	foreign := ForkContext{OriginalBody: body, SourceFormat: "anthropic", SourceTarget: "https://anthropic-compatible.example"}
	headers, key, usable := s.forkAuthFor(foreign, "https://anthropic-compatible.example", false)
	if key != "" || headers != nil || !usable {
		t.Errorf("fork got credential (headers=%v key=%q) for a foreign Anthropic-compatible origin", headers, key)
	}

	// The configured upstream still uses the proxy's own key.
	own := ForkContext{OriginalBody: body, SourceFormat: "anthropic", SourceTarget: "https://api.anthropic.com"}
	if _, key, _ := s.forkAuthFor(own, "https://api.anthropic.com", false); key != "sk-ant-secret" {
		t.Errorf("fork at the configured upstream lost the proxy key, got %q", key)
	}
}

// YM-326 Sicherheits-Review: eine URL ohne Host hat keine Origin, die man
// vergleichen koennte; die Grenze muss dort schliessen.
func TestLos39c_ForkSameOriginRequiresHost(t *testing.T) {
	for _, pair := range [][2]string{
		{"api.anthropic.com", "evil.example"},
		{"https:api.anthropic.com", "https:evil.example"},
		{"localhost:9099", "localhost:1234"},
	} {
		if forkSameOrigin(pair[0], pair[1]) {
			t.Errorf("forkSameOrigin(%q, %q) = true, want false (no host to compare)", pair[0], pair[1])
		}
	}
	if !forkSameOrigin("https://api.z.ai/x", "HTTPS://API.Z.AI/y") {
		t.Error("same host must compare equal regardless of case and path")
	}
}

// YM-326 Befund 1 (zweite Haelfte) und Review-Befund F1: ohne Client-Credential
// setzte doForkCall den Anthropic-Key als Bearer an ein OpenAI-Ziel. Der Key
// gehoert nicht dorthin. Ein Ziel, das gar kein Credential braucht (lokaler
// OpenAI-kompatibler Server), muss aber erreichbar bleiben: er bekommt den Fork
// ohne Authorization, genau wie der Client-Request selbst.
func TestLos39c_ForkToCredentialFreeTargetSendsNoAuth(t *testing.T) {
	src := los39cNewForkServer(t)

	s := &Server{
		cfg: Config{
			TargetURL:       src.URL,
			OpenAITargetURL: src.URL,
			APIKey:          "sk-ant-secret",
		},
		httpClient: src.Client(),
		logger:     log.New(io.Discard, "", 0),
		forkState:  NewForkState(0, 0, 3, 10),
	}
	s.forkConfigs = []ForkConfig{{
		Name:      "los39c_no_client_auth",
		MaxTokens: 64,
		Prompt:    func(ForkContext) string { return "[FORK]" },
	}}

	los39cFireFork(t, s, false)

	select {
	case auth := <-src.forkAuth:
		if auth != "" {
			t.Fatalf("fork sent %q to an OpenAI-compatible endpoint: the Anthropic key is not a credential there", auth)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fork did not run against a credential-free target")
	}
}

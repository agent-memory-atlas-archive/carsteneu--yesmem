package proxy

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A failed OpenAI upstream call reached the client as 502 and left no journal
// line: deepseek, z.ai and glm errors stayed invisible to proxy_health, which
// counts "upstream error:" lines. The client answer is not the diagnostic the
// health check reads, so the failure has to be logged where it happens.
func TestLos32fOpenAITransportErrorIsLogged(t *testing.T) {
	var logged bytes.Buffer

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadURL := "http://" + ln.Addr().String()
	// No listener behind the address: the dial is refused, which is the same
	// class of failure proxy_health counts as a connection error.
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	s := &Server{
		cfg:        Config{OpenAITargetURL: deadURL},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     log.New(&logged, "", 0),
	}
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.forwardOpenAIWithTracking(w, req, body, 1, nil, "yesmem", "", 0, 1, false, nil)

	if got := w.Result().StatusCode; got != http.StatusBadGateway {
		t.Errorf("client status = %d, want %d", got, http.StatusBadGateway)
	}
	if !strings.Contains(logged.String(), "upstream error:") {
		t.Errorf("a refused OpenAI upstream left no log line, journal was:\n%s", logged.String())
	}
}

// An upstream 4xx/5xx with an empty body is a real provider failure, but the
// OpenAI path logs no status at all and the Anthropic path only logs a body it
// received. Both paths must name the status, or the failure stays invisible.
func TestLos32fEmptyUpstreamErrorBodyIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		forwar func(*Server, http.ResponseWriter, *http.Request)
	}{
		{
			name: "openai path",
			forwar: func(s *Server, w http.ResponseWriter, r *http.Request) {
				body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`)
				s.forwardOpenAIWithTracking(w, r, body, 1, nil, "yesmem", "", 0, 1, false, nil)
			},
		},
		{
			name: "anthropic path",
			forwar: func(s *Server, w http.ResponseWriter, r *http.Request) {
				body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)
				s.forwardWithAnnotation(w, r, body, 1, nil, "yesmem", "/home/tester/memory/yesmem", "", 1, false)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer upstream.Close()

			var logged bytes.Buffer
			s := &Server{
				cfg:        Config{TargetURL: upstream.URL, OpenAITargetURL: upstream.URL},
				httpClient: http.DefaultClient,
				logger:     log.New(&logged, "", 0),
			}
			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(nil))
			w := httptest.NewRecorder()

			tc.forwar(s, w, req)

			if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
				t.Errorf("client status = %d, want %d passed through", got, http.StatusServiceUnavailable)
			}
			if !strings.Contains(logged.String(), "upstream 503") {
				t.Errorf("an empty 503 body left no log line, journal was:\n%s", logged.String())
			}
		})
	}
}

// The journal line is also a health signal: proxy_health counts "upstream error:"
// lines and alarms past a threshold, so a request the client itself abandoned
// must not read as a provider failure.
func TestLos32fCancelledRequestIsNotAnUpstreamError(t *testing.T) {
	var logged bytes.Buffer
	s := &Server{
		cfg:        Config{OpenAITargetURL: "http://127.0.0.1:1"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     log.New(&logged, "", 0),
	}
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()

	s.forwardOpenAIWithTracking(w, req, body, 1, nil, "yesmem", "", 0, 1, false, nil)

	if strings.Contains(logged.String(), "upstream error:") {
		t.Errorf("a cancelled request was logged as a provider failure:\n%s", logged.String())
	}
	if got := w.Result().StatusCode; got != http.StatusBadGateway {
		t.Errorf("client status = %d, want %d", got, http.StatusBadGateway)
	}
}

// A body that breaks off mid-read is the same class: the call failed and the
// health check needs the line. The wording matters, because proxy_health reads
// it: "upstream error:" counts as a provider failure, while a bare "read error:"
// with an i/o timeout landed in the daemon-timeout bucket instead.
func TestLos32fOpenAIReadErrorIsLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-1"`))
	}))
	defer upstream.Close()

	var logged bytes.Buffer
	s := &Server{
		cfg:        Config{OpenAITargetURL: upstream.URL},
		httpClient: http.DefaultClient,
		logger:     log.New(&logged, "", 0),
	}
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.forwardOpenAIWithTracking(w, req, body, 1, nil, "yesmem", "", 0, 1, false, nil)

	if !strings.Contains(logged.String(), "upstream error:") {
		t.Errorf("a truncated upstream body left no provider-failure line, journal was:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "read from upstream") {
		t.Errorf("the line does not say the body broke off, journal was:\n%s", logged.String())
	}
}

// An id keeps its alphabet and loses everything else: a bracket ends the
// "[req N tid=...] " prefix early and lets the id plant a status header.
func TestLos32fJournalTokenDropsStructuralCharacters(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"session id":       {"opencode:ses_abc-12.3", "opencode:ses_abc-12.3"},
		"bracket":          {"a] upstream 529:", "aupstream529:"},
		"space":            {"a b", "ab"},
		"newline":          {"a\nb", "ab"},
		"quote and escape": {`a"b\x1b`, "abx1b"},
		// The provider separator survives, otherwise the routing warning names a
		// model the operator cannot look up (YM-440).
		"provider-qualified model": {"zai-coding-plan/glm-5.2", "zai-coding-plan/glm-5.2"},
		// What the cap counts is "upstream 429:" with a space, and a token never
		// keeps one, so the slash does not hand the counter anything back.
		"counted upstream code": {"upstream 429:", "upstream429:"},
	} {
		if got := journalToken(tc.in); got != tc.want {
			t.Errorf("%s: journalToken(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// The journal is a line sink that also reaches terminals and editors, so the
// upstream body is rendered without control characters: a bare \n or \r breaks
// the line, and ESC or a Unicode line separator corrupts every reader of the
// file.
func TestLos32fUpstreamBodyStaysOnOneLine(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("boom\nupstream 529: forged overload\n\x1b[2J\x07\u2028still here"))
	}))
	defer upstream.Close()

	var logged bytes.Buffer
	s := &Server{
		cfg:        Config{TargetURL: upstream.URL, OpenAITargetURL: upstream.URL},
		httpClient: http.DefaultClient,
		logger:     log.New(&logged, "", 0),
	}
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.forwardOpenAIWithTracking(w, req, body, 1, nil, "yesmem", "", 0, 1, false, nil)

	lines := strings.Split(strings.TrimRight(logged.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("journal got %d lines from one upstream error, want 1:\n%q", len(lines), logged.String())
	}
	if !strings.Contains(lines[0], "boom") || !strings.Contains(lines[0], "upstream 502") {
		t.Errorf("log line = %q, want the status and the body", lines[0])
	}
	// The colour prefix and suffix are this proxy's own, so only the sequences
	// that came in with the body are checked.
	for _, forbidden := range []string{"\x1b[2J", "\x07", "\u2028"} {
		if strings.Contains(lines[0], forbidden) {
			t.Errorf("log line carries the body's control sequence %q: %q", forbidden, lines[0])
		}
	}
}

// The thread id comes from the request body (metadata.user_id) and is not
// validated on the way in: a newline in it would append a forged journal line
// that carries counter vocabulary. A set thread id also starts stream tracking
// against the daemon, so the sanitiser is pinned directly instead.
func TestLos32fOneLineForJournalStripsControlCharacters(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string
	}{
		"newline":         {"a\nb", "a b"},
		"carriage return": {"a\rb", "a b"},
		"escape sequence": {"a\x1b[2Jb", "a [2Jb"},
		"bell":            {"a\x07b", "a b"},
		"c1 control":      {"a\u0085b", "a b"},
		"line separator":  {"a\u2028b", "a b"},
		"paragraph break": {"a\u2029b", "a b"},
		"printable stays": {"ses_ok-42", "ses_ok-42"},
	} {
		if got := oneLineForJournal(tc.in); got != tc.want {
			t.Errorf("%s: oneLineForJournal(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// The request URL is built from the client's own query, so a key passed there
// would reach the journal through the transport error. Only the path is
// evidence, the query is not.
func TestLos32fTransportErrorLogDropsTheQuery(t *testing.T) {
	var logged bytes.Buffer
	s := &Server{
		cfg:        Config{OpenAITargetURL: "http://127.0.0.1:1"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     log.New(&logged, "", 0),
	}
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions?api_key=sk-live-secret", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.forwardOpenAIWithTracking(w, req, body, 1, nil, "yesmem", "", 0, 1, false, nil)

	if strings.Contains(logged.String(), "sk-live-secret") {
		t.Errorf("the query reached the journal:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "connection refused") {
		t.Errorf("the line lost the cause, journal was:\n%s", logged.String())
	}
}

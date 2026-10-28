package proxy

// Shared fixtures for the proxy package. The socket stub used to exist four
// times (fakeDaemon, los33DaemonSocket, fakeDaemonDir, los56cDaemonSock); the
// listen-accept-decode-reply loop now lives once, and the four call shapes stay
// as thin policies over it.

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// shortSocketDir returns a temp dir short enough to hold a unix socket on
// darwin. macOS caps sun_path at 104 bytes, and t.TempDir() nests the test name
// under /var/folders/<...>/T, which overruns that limit: bind and connect then
// fail with "invalid argument" before the test logic ever runs.
func shortSocketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "ym")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startFakeDaemon serves the daemon JSON-RPC protocol on <dir>/daemon.sock and
// returns the dir. reply maps method+params to the result JSON; an empty or
// unparseable result becomes null, which is what a caller sees for a method the
// stub does not answer. The stub dies with the test.
func startFakeDaemon(t *testing.T, reply func(method string, params map[string]any) string) string {
	t.Helper()

	dir := shortSocketDir(t)
	ln, err := net.Listen("unix", filepath.Join(dir, "daemon.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeDaemonConn(conn, reply)
		}
	}()
	return dir
}

func serveFakeDaemonConn(conn net.Conn, reply func(method string, params map[string]any) string) {
	defer conn.Close()

	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	result := json.RawMessage(reply(req.Method, req.Params))
	if !json.Valid(result) {
		result = json.RawMessage("null")
	}
	_ = json.NewEncoder(conn).Encode(struct {
		Result json.RawMessage `json:"result"`
	}{Result: result})
}

// fakeDaemon returns a Server wired to a daemon stub, so tests can exercise
// queryDaemon callers hermetically.
func fakeDaemon(t *testing.T, handler func(method string, params map[string]any) string) *Server {
	t.Helper()

	s := &Server{cfg: Config{DataDir: startFakeDaemon(t, handler)}, logger: log.New(io.Discard, "", 0)}
	s.daemonReady.Store(true)
	return s
}

// wireFakeDaemon points an already built Server at a daemon stub. Its own state
// (logger, feature gates, maps) stays as the test wired it.
func wireFakeDaemon(t *testing.T, s *Server, handler func(method string, params map[string]any) string) {
	t.Helper()

	s.cfg.DataDir = startFakeDaemon(t, handler)
	s.daemonReady.Store(true)
}

// fakeDaemonDir serves canned per-method results; an unknown method answers {}
// so a caller that probes an unwired method sees an empty result.
func fakeDaemonDir(t *testing.T, responses map[string]json.RawMessage) string {
	t.Helper()

	return startFakeDaemon(t, func(method string, _ map[string]any) string {
		if result, ok := responses[method]; ok {
			return string(result)
		}
		return `{}`
	})
}

// recordingUpstream is a recording backend: it captures the forwarded request
// body so a test can assert on what the proxy actually sent upstream.
func recordingUpstream(t *testing.T) (string, func() string) {
	t.Helper()
	var mu sync.Mutex
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = string(raw)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() string {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// userTurnBody is a minimal real user turn. A thinking block and more than one
// message are what keep the request out of the subagent path.
func userTurnBody(model string) string {
	return `{"model":"` + model + `","stream":false,"thinking":{"type":"adaptive"},` +
		`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
		`"messages":[{"role":"user","content":"start the task"},` +
		`{"role":"assistant","content":"on it"},` +
		`{"role":"user","content":"check the prompt profile routing"}]}`
}

// messagesRequest builds the /v1/messages request with the given headers.
func messagesRequest(body string, headers map[string]string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

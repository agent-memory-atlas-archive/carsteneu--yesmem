package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// los33Upstream is a recording backend: it captures the forwarded request body
// so a test can assert on what the proxy actually sent upstream.
func los33Upstream(t *testing.T) (string, func() string) {
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

// los33DaemonSocket points an existing test server at a unix-socket daemon stub,
// reusing serveFakeDaemonConn from fake_daemon_test.go.
func los33DaemonSocket(t *testing.T, s *Server, handler func(method string, params map[string]any) string) {
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
			go serveFakeDaemonConn(conn, handler)
		}
	}()

	s.cfg.DataDir = dir
	s.daemonReady.Store(true)
}

// los33UserTurn is a minimal real user turn. A thinking block and more than one
// message are what keep the request out of the subagent path.
func los33UserTurn(model string) string {
	return `{"model":"` + model + `","stream":false,"thinking":{"type":"adaptive"},` +
		`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
		`"messages":[{"role":"user","content":"start the task"},` +
		`{"role":"assistant","content":"on it"},` +
		`{"role":"user","content":"check the prompt profile routing"}]}`
}

// los33MessagesRequest builds the /v1/messages request with the given headers.
func los33MessagesRequest(body string, headers map[string]string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

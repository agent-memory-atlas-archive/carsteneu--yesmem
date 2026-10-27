package proxy

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"path/filepath"
	"testing"
)

// fakeDaemon starts a unix-socket daemon stub in t.TempDir() and returns a
// Server wired to it, so tests can exercise queryDaemon callers hermetically.
// handler maps method+params to the raw result JSON ("null" for an empty
// result). The stub outlives the test only until t.Cleanup closes the listener.
func fakeDaemon(t *testing.T, handler func(method string, params map[string]any) string) *Server {
	t.Helper()

	dir := t.TempDir()
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

	s := &Server{cfg: Config{DataDir: dir}, logger: log.New(io.Discard, "", 0)}
	s.daemonReady.Store(true)
	return s
}

func serveFakeDaemonConn(conn net.Conn, handler func(method string, params map[string]any) string) {
	defer conn.Close()

	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	result := handler(req.Method, req.Params)
	if result == "" {
		result = "null"
	}
	var payload any
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		payload = nil
	}
	_ = json.NewEncoder(conn).Encode(map[string]any{"result": payload})
}

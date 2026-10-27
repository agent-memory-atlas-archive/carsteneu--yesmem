package mcp

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// fakeDaemonError answers every call with a daemon-level Response.Error, the
// shape the daemon produces for an application error (socket.go: "daemon: %s").
// A reply of this kind means the daemon is alive and the connection is fine.
func fakeDaemonError(t *testing.T, errText string) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if err := json.NewDecoder(c).Decode(&req); err != nil {
					return
				}
				json.NewEncoder(c).Encode(map[string]string{"error": errText})
			}(conn)
		}
	}()
	return sockPath
}

func callToolHandlerLOS11i(t *testing.T, h mcpserver.ToolHandlerFunc) string {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "remember"
	req.Params.Arguments = map[string]any{"text": "x"}
	result, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if len(result.Content) == 0 {
		return ""
	}
	if text, ok := result.Content[0].(mcplib.TextContent); ok {
		return text.Text
	}
	return ""
}

// TestLos11iProxyFormatWithParamsKeepsProxyOnDaemonError is the regression test
// for YM-345 (1): a daemon-level error is an application error, not a broken
// connection. The daemon answered, so throwing the proxy away forces a pointless
// reconnect (and a possible daemon start attempt) on the next call.
func TestLos11iProxyFormatWithParamsKeepsProxyOnDaemonError(t *testing.T) {
	t.Setenv("YESMEM_SOCK", fakeDaemonError(t, "remember: text is required"))

	s := &Server{dataDir: t.TempDir()}
	s.proxy = &ProxyClient{dataDir: s.dataDir}

	out := callToolHandlerLOS11i(t, s.proxyCallFormatWithParams("remember", nil))
	if !strings.Contains(out, "daemon: remember: text is required") {
		t.Fatalf("daemon error did not reach the caller, got %q", out)
	}
	if s.proxy == nil {
		t.Fatal("proxy was reset after a daemon-level error; the connection is fine, only the query failed")
	}
}

// TestLos11iProxyFormatWithParamsResetsProxyOnConnectionError is the counter-test:
// a transport error must still drop the proxy so the next call reconnects.
func TestLos11iProxyFormatWithParamsResetsProxyOnConnectionError(t *testing.T) {
	t.Setenv("YESMEM_SOCK", filepath.Join(t.TempDir(), "absent.sock"))

	s := &Server{dataDir: t.TempDir()}
	s.proxy = &ProxyClient{dataDir: s.dataDir}

	out := callToolHandlerLOS11i(t, s.proxyCallFormatWithParams("remember", nil))
	if !strings.Contains(out, "Error:") {
		t.Fatalf("expected an error result, got %q", out)
	}
	if s.proxy != nil {
		t.Fatal("proxy survived a connection error; the next call would reuse a dead connection")
	}
}

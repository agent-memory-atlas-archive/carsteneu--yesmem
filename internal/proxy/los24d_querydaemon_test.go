package proxy

import (
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The other side of the contract: a socket that does not exist yet belongs to a
// daemon that is still starting, and the cold-start budget must keep waiting for
// it. A fix that fails fast on every dial error would break the proxy's own start.
func TestQueryDaemonWaitsForAColdStartingDaemon(t *testing.T) {
	dir := shortSocketDir(t)
	s := &Server{cfg: Config{DataDir: dir}, logger: log.New(io.Discard, "", 0)}

	go func() {
		time.Sleep(700 * time.Millisecond)
		ln, err := net.Listen("unix", filepath.Join(dir, "daemon.sock"))
		if err != nil {
			return
		}
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		serveFakeDaemonConn(conn, func(string, map[string]any) string {
			return `{"value":"ok"}`
		})
	}()

	start := time.Now()
	result, err := s.queryDaemon("get_proxy_state", map[string]any{"key": "k"})
	if err != nil {
		t.Fatalf("queryDaemon gave up before the daemon bound its socket: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("queryDaemon returned after %v, before the socket existed", elapsed)
	}
	if string(result) != `{"value":"ok"}` {
		t.Errorf("result = %s", result)
	}
}

// YM-338 (Los 24d): on a cold start every RPC retried the socket for 15 seconds,
// even when the daemon was dead. A stale socket file (the daemon is gone, the
// path it bound is still there) is exactly that case: dialing it fails at once
// with ECONNREFUSED, and waiting longer cannot change the answer. Only a MISSING
// socket can belong to a daemon that is still starting.
func TestQueryDaemonFailsFastOnADeadSocket(t *testing.T) {
	dir := shortSocketDir(t)
	sockPath := filepath.Join(dir, "daemon.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Leave the socket file behind, the way a killed daemon does.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("precondition: the stale socket file is gone: %v", err)
	}

	s := &Server{cfg: Config{DataDir: dir}, logger: log.New(io.Discard, "", 0)}

	start := time.Now()
	if _, err := s.queryDaemon("get_proxy_state", map[string]any{"key": "k"}); err == nil {
		t.Fatal("queryDaemon reported success against a dead socket")
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("queryDaemon waited %v for a socket nobody listens on; the cold-start budget belongs to a daemon that is still starting", elapsed)
	}
}

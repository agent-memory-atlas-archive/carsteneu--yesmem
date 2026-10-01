package proxy

import (
	"net"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestPeerSessionPID_ResolvesDialerPID spins up a real loopback connection.
// The resolver must identify the peer endpoint's owning process — which, in
// this test, is the test process itself (client socket and resolver share /proc).
func TestPeerSessionPID_ResolvesDialerPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("/proc-based pid resolution is Linux-only (GOOS=%s)", runtime.GOOS)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listen: %v", err)
	}
	defer ln.Close()

	type result struct {
		addr net.Addr
		err  error
	}
	got := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- result{err: err}
			return
		}
		got <- result{addr: conn.RemoteAddr()}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	res := <-got
	if res.err != nil {
		t.Fatalf("accept: %v", res.err)
	}

	pid := peerSessionPID(ln.Addr().String(), res.addr.String())
	if pid != os.Getpid() {
		t.Fatalf("peerSessionPID(%s) = %d, want own pid %d", res.addr.String(), pid, os.Getpid())
	}

	// Cache: zweiter Aufruf gleicher Addr → gleiche PID
	if pid2 := peerSessionPID(ln.Addr().String(), res.addr.String()); pid2 != os.Getpid() {
		t.Fatalf("cached second call = %d, want %d", pid2, os.Getpid())
	}
}

func TestPeerSessionPID_GarbageAddr(t *testing.T) {
	if got := peerSessionPID("127.0.0.1:9099", "not-an-addr"); got != 0 {
		t.Fatalf("peerSessionPID(garbage) = %d, want 0", got)
	}
}

// TestPeerPIDCachePrune hält die Cache-Map klein: alte Einträge fliegen.
func TestPeerPIDCachePrune(t *testing.T) {
	c := newPeerPIDCache()
	c.mu.Lock()
	c.m["127.0.0.1:1111"] = peerPIDEntry{pid: 1, inode: 11, seen: time.Now().Add(-3 * time.Hour)}
	c.m["127.0.0.1:2222"] = peerPIDEntry{pid: 2, inode: 22, seen: time.Now()}
	c.mu.Unlock()

	c.prune()
	c.mu.Lock()
	_, old := c.m["127.0.0.1:1111"]
	_, neu := c.m["127.0.0.1:2222"]
	c.mu.Unlock()
	if old {
		t.Fatal("stale entry not pruned")
	}
	if !neu {
		t.Fatal("fresh entry pruned incorrectly")
	}
}

package proxy

import (
	"net"
	"os"
	"runtime"
	"testing"
	"time"
)

// YM-271 [AGT-MA36] key 3: peerPIDCache.prune() existed but had no caller in the
// product. The map therefore only grew: one entry per loopback endpoint the proxy
// ever saw, and a recycled port kept its old owner until a fresh resolution
// overwrote the key.
func TestPeerSessionPIDPrunesStaleEntriesLos55a(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("/proc-based pid resolution is Linux-only (GOOS=%s)", runtime.GOOS)
	}
	prev := defaultPeerPIDs
	defaultPeerPIDs = newPeerPIDCache()
	t.Cleanup(func() { defaultPeerPIDs = prev })

	stale := peerPIDEntry{pid: 1, inode: 11, seen: time.Now().Add(-3 * time.Hour)}
	defaultPeerPIDs.mu.Lock()
	defaultPeerPIDs.m["127.0.0.1:1"] = stale
	defaultPeerPIDs.mu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	// Hold the accepted side open: a dropped conn can sit in CLOSE_WAIT, and the
	// resolver only accepts an ESTABLISHED row, so the fixture would flake to pid 0.
	server := <-accepted
	defer server.Close()
	remote := server.RemoteAddr()

	if pid := peerSessionPID(ln.Addr().String(), remote.String()); pid != os.Getpid() {
		t.Fatalf("peerSessionPID = %d, want own pid %d", pid, os.Getpid())
	}

	defaultPeerPIDs.mu.Lock()
	_, stillThere := defaultPeerPIDs.m["127.0.0.1:1"]
	defaultPeerPIDs.mu.Unlock()
	if stillThere {
		t.Error("a three-hour-old entry survived a resolution — prune() still has no caller")
	}
}

// A long-lived keep-alive connection is the normal case for opencode, so a cache
// hit has to count as "seen". Without it, prune() deletes a live entry and the next
// request pays a full /proc scan for nothing. The refresh happens in the same step
// as the inode check (cold review: a split read-then-write could resurrect a stale
// answer over a newer one).
func TestPeerPIDCacheHitRefreshesSeenLos55a(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("/proc-based pid resolution is Linux-only (GOOS=%s)", runtime.GOOS)
	}
	prev := defaultPeerPIDs
	defaultPeerPIDs = newPeerPIDCache()
	t.Cleanup(func() { defaultPeerPIDs = prev })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	// Hold the accepted side open: a dropped conn can sit in CLOSE_WAIT, and the
	// resolver only accepts an ESTABLISHED row, so the fixture would flake to pid 0.
	server := <-accepted
	defer server.Close()
	remote := server.RemoteAddr()

	if pid := peerSessionPID(ln.Addr().String(), remote.String()); pid != os.Getpid() {
		t.Fatalf("peerSessionPID = %d, want own pid %d", pid, os.Getpid())
	}

	// Age the entry, then resolve again: the second call is a cache hit.
	old := time.Now().Add(-90 * time.Minute)
	defaultPeerPIDs.mu.Lock()
	e := defaultPeerPIDs.m[remote.String()]
	e.seen = old
	defaultPeerPIDs.m[remote.String()] = e
	defaultPeerPIDs.mu.Unlock()

	if pid := peerSessionPID(ln.Addr().String(), remote.String()); pid != os.Getpid() {
		t.Fatalf("cached peerSessionPID = %d, want own pid %d", pid, os.Getpid())
	}

	defaultPeerPIDs.mu.Lock()
	seen := defaultPeerPIDs.m[remote.String()].seen
	defaultPeerPIDs.mu.Unlock()
	if !seen.After(old) {
		t.Errorf("seen = %v, want it refreshed past %v on a cache hit", seen, old)
	}
}

// remember() is the write path, so it has to bound the map: an entry written for
// one endpoint must not carry every stale endpoint along.
func TestPeerPIDCacheRememberDropsStaleLos55a(t *testing.T) {
	c := newPeerPIDCache()
	c.mu.Lock()
	c.m["127.0.0.1:4444"] = peerPIDEntry{pid: 2, inode: 22, seen: time.Now().Add(-3 * time.Hour)}
	c.mu.Unlock()

	c.remember("127.0.0.1:5555", peerPIDEntry{pid: 3, inode: 33, seen: time.Now()})

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m["127.0.0.1:4444"]; ok {
		t.Error("remember() kept a stale entry, want it pruned on the write")
	}
	if _, ok := c.m["127.0.0.1:5555"]; !ok {
		t.Error("remember() did not store the new entry")
	}
}

package proxy

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

// los26eResetCacheRecorder is a daemon stub that records the prefixes of every
// delete_proxy_state_prefix call. The loop prefix is deliberate: parallel loops
// in this package have collided on helper names before.
type los26eResetCacheRecorder struct {
	mu       sync.Mutex
	prefixes []string
}

func (r *los26eResetCacheRecorder) handler(method string, params map[string]any) string {
	if method != "delete_proxy_state_prefix" {
		return "null"
	}
	prefix, _ := params["prefix"].(string)
	r.mu.Lock()
	r.prefixes = append(r.prefixes, prefix)
	r.mu.Unlock()
	return `{"deleted":2}`
}

func (r *los26eResetCacheRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prefixes...)
}

// --reset-cache had no test of its own: the branch sat inline in Run, and the
// two Run-based integration tests in this package never set ResetCache.
// Extracting it makes the contract testable: one delete_proxy_state_prefix call
// per family, in a fixed order.
func TestLos26e_ResetCacheClearsPersistedCacheFamilies(t *testing.T) {
	rec := &los26eResetCacheRecorder{}
	s := fakeDaemon(t, rec.handler)

	s.los26eResetPersistedCacheState()

	want := []string{"frozen:", "frozen_markers:", "decay:", "timestamps:"}
	got := rec.seen()
	if len(got) != len(want) {
		t.Fatalf("reset sent %d prefix deletions %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("prefix %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The reset promises that one unreachable family does not abort the rest. The
// stub hangs up on "decay:" without answering, which queryDaemon reports as a
// transport error; every later family must still be requested.
func TestLos26e_ResetCacheContinuesAfterFailedFamily(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "daemon.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var seen []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				conn.Close()
				continue
			}
			prefix, _ := req.Params["prefix"].(string)
			mu.Lock()
			seen = append(seen, prefix)
			mu.Unlock()
			if prefix == "decay:" {
				conn.Close() // no response: the caller sees a transport error
				continue
			}
			_ = json.NewEncoder(conn).Encode(map[string]any{"result": map[string]any{"deleted": 1}})
			conn.Close()
		}
	}()

	s := &Server{cfg: Config{DataDir: dir}, logger: log.New(io.Discard, "", 0)}
	s.daemonReady.Store(true)

	s.los26eResetPersistedCacheState()

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	want := []string{"frozen:", "frozen_markers:", "decay:", "timestamps:"}
	if len(got) != len(want) {
		t.Fatalf("reset requested %v, want all of %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, got[i], want[i])
		}
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
)

// los55cFakeSocket answers like the daemon's socket and reports every request
// it saw. The loop prefix keeps the helper apart from other loops' fakes.
func los55cFakeSocket(t *testing.T) chan map[string]any {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv("YESMEM_SOCK", socketPath)

	received := make(chan map[string]any, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req map[string]any
				if err := json.NewDecoder(c).Decode(&req); err != nil {
					return
				}
				received <- req
				json.NewEncoder(c).Encode(map[string]any{"result": map[string]any{"status": "ok"}})
			}(conn)
		}
	}()
	return received
}

// YM-268 [CFG-03] (Los 55c), Befund 1: `yesmem config set` schrieb nur die
// Datei. Der Proxy und get_config lesen zuerst den Runtime-Slot in
// proxy_state, also blieb ein CLI-Write unsichtbar: get_config antwortete
// weiter mit dem alten Wert, und der Proxy wandte ihn an.
func TestLos55c_ConfigSetMirrorsTheRuntimeSlot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	received := los55cFakeSocket(t)

	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	cfgPath := filepath.Join(dataDir, "config.yaml")

	if err := setConfigValue(dataDir, cfgPath, "proxy.token_threshold", "300000"); err != nil {
		t.Fatalf("set global proxy key: %v", err)
	}
	if err := setConfigValue(dataDir, cfgPath, "proxy.token_thresholds.deepseek", "500000"); err != nil {
		t.Fatalf("set per-model proxy key: %v", err)
	}
	if err := setConfigValue(dataDir, cfgPath, "extraction.model", "opus"); err != nil {
		t.Fatalf("set non-proxy key: %v", err)
	}

	for _, want := range []struct{ key, value string }{
		{"config_override:token_threshold", "300000"},
		{"config_override:token_threshold:deepseek", "500000"},
	} {
		select {
		case req := <-received:
			if method := fmt.Sprintf("%v", req["method"]); method != "set_proxy_state" {
				t.Fatalf("runtime slot updated with %q, want set_proxy_state", method)
			}
			params, _ := req["params"].(map[string]any)
			if got := fmt.Sprintf("%v", params["key"]); got != want.key {
				t.Errorf("runtime slot key = %q, want %q", got, want.key)
			}
			if got := fmt.Sprintf("%v", params["value"]); got != want.value {
				t.Errorf("runtime slot value = %q, want %q", got, want.value)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("config set wrote no runtime slot for %s: the proxy keeps the previous value", want.key)
		}
	}

	// A key without a runtime reader must not produce a daemon call.
	select {
	case req := <-received:
		t.Errorf("unexpected daemon request for a non-proxy key: %v", req)
	case <-time.After(200 * time.Millisecond):
	}
}

// The file is the primary store: an unreachable daemon may cost the runtime
// update, but not the write.
func TestLos55c_ConfigSetStillWritesTheFileWithoutADaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("YESMEM_SOCK", filepath.Join(t.TempDir(), "absent.sock"))

	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	cfgPath := filepath.Join(dataDir, "config.yaml")

	if err := setConfigValue(dataDir, cfgPath, "proxy.effort_floor", "high"); err != nil {
		t.Fatalf("a daemon that is not running must not fail the write: %v", err)
	}
	got, err := config.GetValue(cfgPath, "proxy.effort_floor")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if fmt.Sprintf("%v", got) != "high" {
		t.Errorf("proxy.effort_floor = %v, want high", got)
	}
}

// Cold review finding 1: only the token-threshold family is re-read by the
// running proxy. Mirroring any other proxy key writes a slot that the proxy
// never applies, and get_config then reports a value that is not in effect.
func TestLos55c_ConfigSetDoesNotMirrorKeysWithoutARuntimeReader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	received := los55cFakeSocket(t)

	dataDir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	cfgPath := filepath.Join(dataDir, "config.yaml")

	if err := setConfigValue(dataDir, cfgPath, "proxy.effort_floor", "high"); err != nil {
		t.Fatalf("set proxy.effort_floor: %v", err)
	}

	select {
	case req := <-received:
		t.Errorf("proxy.effort_floor was mirrored into the runtime slot, but no proxy reader consumes it: %v", req)
	case <-time.After(300 * time.Millisecond):
	}

	got, err := config.GetValue(cfgPath, "proxy.effort_floor")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if fmt.Sprintf("%v", got) != "high" {
		t.Errorf("proxy.effort_floor = %v, want high: the file is still the store", got)
	}
}

package daemon

import (
	"encoding/json"
	"os"
	"testing"
)

// The channel endpoints decide ownership from the peer process of the
// connection, so the socket layer has to report the pid the kernel gives it
// (SO_PEERCRED). It is a fact about the connection, unlike the _caller_pid a
// client asserts, and it is overwritten on every request — a client that sends
// the key itself cannot keep its own value.
func TestSocketServer_AttachesThePeerPID(t *testing.T) {
	dir := t.TempDir()
	seen := make(chan any, 1)
	srv, err := NewSocketServer(dir, func(req Request) Response {
		seen <- req.Params["_peer_pid"]
		return Response{Result: json.RawMessage(`{"ok":true}`)}
	})
	if err != nil {
		t.Fatalf("NewSocketServer: %v", err)
	}
	defer srv.Close()
	go srv.Serve()

	client, err := Dial(dir)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	// A caller that presents its own _peer_pid must not keep it.
	if _, err := client.Call("whoami", map[string]any{"session_id": "x", "_peer_pid": 1}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	got := <-seen
	pid, ok := got.(int)
	if !ok || pid != os.Getpid() {
		t.Errorf("peer pid = %v (%T), want %d", got, got, os.Getpid())
	}
}

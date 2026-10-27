package hooks

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunSessionEnd_CallsDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("", "t")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sockPath := filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var receivedMethod string
	var receivedParams map[string]any
	done := make(chan struct{})

	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		json.NewDecoder(conn).Decode(&req)
		receivedMethod = req.Method
		receivedParams = req.Params
		resp := map[string]any{"result": map[string]any{"status": "tracked"}}
		json.NewEncoder(conn).Encode(resp)
	}()

	input := `{"session_id":"sess-abc","cwd":"/home/testuser/projects/myapp","reason":"clear"}`
	r, w, _ := os.Pipe()
	w.Write([]byte(input))
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	oldStdout := os.Stdout
	rOut, wOut, _ := os.Pipe()
	os.Stdout = wOut

	RunSessionEnd(dir)

	wOut.Close()
	os.Stdout = oldStdout
	buf := make([]byte, 1024)
	n, _ := rOut.Read(buf)
	output := string(buf[:n])

	// Wait for mock server to finish
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mock server timed out")
	}

	if receivedMethod != "track_session_end" {
		t.Errorf("expected method 'track_session_end', got %q", receivedMethod)
	}
	if receivedParams["session_id"] != "sess-abc" {
		t.Errorf("expected session_id='sess-abc', got %v", receivedParams["session_id"])
	}
	if receivedParams["reason"] != "clear" {
		t.Errorf("expected reason='clear', got %v", receivedParams["reason"])
	}
	if receivedParams["project"] != "/home/testuser/projects/myapp" {
		t.Errorf("expected project path, got %v", receivedParams["project"])
	}
	if output != "{}" {
		t.Errorf("expected '{}' output, got %q", output)
	}
}

// "compact" is not a SessionEnd reason: Claude Code fires PreCompact for it and
// never ends the session, so the hook must not report it. The documented
// SessionEnd reasons are clear, resume, logout, prompt_input_exit and other.
func TestRunSessionEnd_IgnoresReasonsThatDoNotEndASession(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{}, 1)
	go func() {
		if conn, err := ln.Accept(); err == nil {
			conn.Close()
			accepted <- struct{}{}
		}
	}()

	oldStdin := os.Stdin
	oldStdout := os.Stdout
	rOut, wOut, _ := os.Pipe()
	os.Stdout = wOut
	defer func() { os.Stdin = oldStdin; os.Stdout = oldStdout }()

	for _, reason := range []string{"compact", "logout", "prompt_input_exit", "other"} {
		input := `{"session_id":"sess-xyz","cwd":"/tmp/proj","reason":"` + reason + `"}`
		r, w, _ := os.Pipe()
		w.Write([]byte(input))
		w.Close()
		os.Stdin = r
		RunSessionEnd(dir)
	}
	wOut.Close()
	buf := make([]byte, 1024)
	n, _ := rOut.Read(buf)
	if want := strings.Repeat("{}", 4); string(buf[:n]) != want {
		t.Errorf("expected %q output, got %q", want, string(buf[:n]))
	}

	select {
	case <-accepted:
		t.Error("the hook contacted the daemon for a reason that does not end a session")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestRunSessionEnd_SkipsNonClearCompact(t *testing.T) {
	dir := t.TempDir()

	input := `{"session_id":"sess-abc","cwd":"/tmp","reason":"logout"}`
	r, w, _ := os.Pipe()
	w.Write([]byte(input))
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	oldStdout := os.Stdout
	rOut, wOut, _ := os.Pipe()
	os.Stdout = wOut

	RunSessionEnd(dir)

	wOut.Close()
	os.Stdout = oldStdout
	buf := make([]byte, 1024)
	n, _ := rOut.Read(buf)
	output := string(buf[:n])

	if output != "{}" {
		t.Errorf("expected '{}' output, got %q", output)
	}
}

func TestRunSessionEnd_DaemonUnreachable(t *testing.T) {
	// No daemon socket — should not hang or crash
	dir := t.TempDir()

	input := `{"session_id":"sess-abc","cwd":"/tmp/proj","reason":"clear"}`
	r, w, _ := os.Pipe()
	w.Write([]byte(input))
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	oldStdout := os.Stdout
	rOut, wOut, _ := os.Pipe()
	os.Stdout = wOut

	RunSessionEnd(dir)

	wOut.Close()
	os.Stdout = oldStdout
	buf := make([]byte, 1024)
	n, _ := rOut.Read(buf)
	output := string(buf[:n])

	if output != "{}" {
		t.Errorf("expected '{}' output, got %q", output)
	}
}

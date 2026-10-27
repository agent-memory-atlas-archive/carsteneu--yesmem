package proxy

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeBriefingDaemon accepts exactly one generate_briefing call on an isolated
// socket and returns the request params. Send-only: a decode failure closes the
// channel so the waiting test fails instead of blocking.
func fakeBriefingDaemon(t *testing.T, dir string) <-chan map[string]any {
	t.Helper()
	sockPath := filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	captured := make(chan map[string]any, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(captured)
			return
		}
		defer conn.Close()
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			close(captured)
			return
		}
		defer close(captured)
		captured <- req.Params
		json.NewEncoder(conn).Encode(map[string]any{
			"result": map[string]any{"text": "briefing", "code_map": ""},
		})
	}()
	return captured
}

func waitBriefingParams(t *testing.T, captured <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case params, ok := <-captured:
		if !ok {
			t.Fatal("fake daemon could not decode the generate_briefing request")
		}
		return params
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for generate_briefing")
		return nil
	}
}

// YM-105: loadBriefing dropped the session identity, so the daemon's agent and
// non-Claude detection (AgentGetAnyBySession, proxy_state "source_agent:<sid>")
// could never fire. The session id sent to generate_briefing must be the same
// key the proxy registers in register_pid / anthropicThreadID.
func TestLoadBriefingSendsSessionID(t *testing.T) {
	dir := t.TempDir()
	captured := fakeBriefingDaemon(t, dir)

	s := &Server{cfg: Config{DataDir: dir}, logger: log.New(io.Discard, "", 0)}
	if data := s.loadBriefing("proj", "/home/tester/proj", "opencode:ses-abc"); data.Text != "briefing" {
		t.Fatalf("loadBriefing text = %q, want %q", data.Text, "briefing")
	}

	params := waitBriefingParams(t, captured)
	if got, _ := params["session_id"].(string); got != "opencode:ses-abc" {
		t.Errorf("generate_briefing session_id = %q, want %q", got, "opencode:ses-abc")
	}
}

// The messages path has no register_pid call, so the agent kind must travel with
// the briefing request; otherwise an opencode session keeps Claude wording.
func TestLoadBriefingSendsSourceAgentForOpenCode(t *testing.T) {
	dir := t.TempDir()
	captured := fakeBriefingDaemon(t, dir)

	s := &Server{cfg: Config{DataDir: dir}, logger: log.New(io.Discard, "", 0)}
	s.loadBriefing("proj", "/home/tester/proj", "opencode:ses-abc")

	params := waitBriefingParams(t, captured)
	if got, _ := params["source_agent"].(string); got != "opencode" {
		t.Errorf("generate_briefing source_agent = %q, want %q", got, "opencode")
	}
}

// Claude Code thread ids are bare session ids: no source_agent, the daemon keeps
// the Claude profile.
func TestLoadBriefingOmitsSourceAgentForClaudeCode(t *testing.T) {
	dir := t.TempDir()
	captured := fakeBriefingDaemon(t, dir)

	s := &Server{cfg: Config{DataDir: dir}, logger: log.New(io.Discard, "", 0)}
	s.loadBriefing("proj", "/home/tester/proj", "5f3c1d9e-1111-2222-3333-444455556666")

	params := waitBriefingParams(t, captured)
	if got, ok := params["source_agent"]; ok {
		t.Errorf("source_agent must be omitted for Claude Code sessions, got %v", got)
	}
}

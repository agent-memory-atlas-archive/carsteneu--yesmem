package proxy

import (
	"net/http/httptest"
	"testing"
)

func TestSubagentStreamInfo_OpenCodeHeaders(t *testing.T) {
	s := &Server{}
	body := []byte(`{"model":"gateway/privateTomMax"}`)

	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Opencode-Agent-Type", "subagent")
	r.Header.Set("X-Opencode-Parent-Session", "ses_abc123")

	isSub, parentThread := s.subagentStreamInfo(r, body)
	if !isSub {
		t.Error("isSub should be true for opencode subagent headers")
	}
	if parentThread != "opencode:ses_abc123" {
		t.Errorf("parentThread = %q, want %q", parentThread, "opencode:ses_abc123")
	}
}

func TestSubagentStreamInfo_MainRequest(t *testing.T) {
	s := &Server{}
	body := []byte(`{"model":"gateway/privateTomMax"}`)

	r := httptest.NewRequest("POST", "/v1/messages", nil)
	isSub, parentThread := s.subagentStreamInfo(r, body)
	if isSub {
		t.Error("isSub should be false without subagent markers")
	}
	if parentThread != "" {
		t.Errorf("parentThread = %q, want empty", parentThread)
	}
}

func TestSubagentStreamInfo_ClaudeCodeBody(t *testing.T) {
	s := &Server{}
	body := []byte(`{"metadata":{"user_id":"{\"agent_type\":\"subagent\"}"}}`)
	r := httptest.NewRequest("POST", "/v1/messages", nil)

	isSub, parentThread := s.subagentStreamInfo(r, body)
	if !isSub {
		t.Error("isSub should be true for claude code subagent body")
	}
	if parentThread != "" {
		t.Errorf("parentThread = %q, want empty", parentThread)
	}
}

package proxy

import (
	"encoding/json"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

// YM-447: the project name is client text. It comes from the working directory
// the request claims ("Primary working directory: ...", "<cwd>...</cwd>"), and it
// is printed into the "[req N <project> tid=...]" prefix of every pipeline line.
// A name carrying a bracket and an error phrase therefore closes the prefix early
// and plants the vocabulary proxy_health counts: four requests were enough for a
// CRITICAL report while no provider call had failed (audit 16.10., PRX-P61).
//
// The writer side is fixed by rendering the slot as a token, the same rule the
// thread id already follows. These tests drive the real handler with a hostile
// working directory and compare it against a clean one.

// los62aHostileProject is the last path segment the handler derives as the
// project name: a space, a bracket and the counting vocabulary the cap reads.
const los62aHostileProject = "p] [req 9 tid=forged] upstream 429: rate_limit"

// los62aHostileWorkdir is the working directory that yields the name above.
const los62aHostileWorkdir = "/home/tester/projects/" + los62aHostileProject

// TestLos62aHostileProjectStaysInsideThePrefix drives /v1/messages twice, once
// with a clean working directory and once with the hostile one, and compares the
// journal. What a line counter sees is the count, so a difference means the
// client moved the health counter; the cap's own selection regex is the second
// assertion, because it is what the report reacts to.
func TestLos62aHostileProjectStaysInsideThePrefix(t *testing.T) {
	up := los61bUpstream(t)

	clean := los62aRunAnthropic(t, up, "/home/tester/projects/los62a-clean")
	hostile := los62aRunAnthropic(t, up, los62aHostileWorkdir)

	cleanLines := los61bLines(clean)
	hostileLines := los61bLines(hostile)
	if len(hostileLines) != len(cleanLines) {
		t.Fatalf("hostile project added %d journal line(s): clean=%d hostile=%d\n%s",
			len(hostileLines)-len(cleanLines), len(cleanLines), len(hostileLines), hostile)
	}
	if !strings.Contains(clean, "[req ") {
		t.Fatalf("clean run reached no journal prefix line, the comparison proves nothing:\n%s", clean)
	}
	los61bAssertNoCapCount(t, hostile)

	// Sanitized, not dropped: the operator still has to see which project sent
	// the request, so the token has to be recognizable in the line.
	if !strings.Contains(hostile, "preq9tidforgedupstream429:rate_limit") {
		t.Fatalf("the project slot lost its value instead of sanitizing it:\n%s", hostile)
	}
	if !strings.Contains(clean, "los62a-clean") {
		t.Fatalf("the clean project name did not survive the fix:\n%s", clean)
	}
}

// los62aRunAnthropic sends one /v1/messages request whose system prompt names
// workdir as the working directory, and returns the journal it wrote.
func los62aRunAnthropic(t *testing.T, up *httptest.Server, workdir string) string {
	t.Helper()

	var buf los61bSyncBuffer
	s := los26aHandlerServer(t, up.URL)
	s.logger = log.New(&buf, "", log.LstdFlags)

	body, err := json.Marshal(map[string]any{
		"model":      "claude-opus-4-6",
		"stream":     false,
		"max_tokens": 16,
		"system": []any{
			map[string]any{"type": "text", "text": "Primary working directory: " + workdir},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "start the task"},
			map[string]any{"role": "assistant", "content": "on it"},
			map[string]any{"role": "user", "content": "check the prompt profile routing"},
		},
	})
	if err != nil {
		t.Fatalf("marshal messages body: %v", err)
	}

	s.handleMessages(httptest.NewRecorder(), messagesRequest(string(body), nil))
	return buf.String()
}

// TestLos62aThreadIdentityIsAToken covers the third route of the ticket: the
// session value becomes the thread key ("opencode:<session>") verbatim, and the
// key is printed in the tid= slot of every line. A value with a space and a
// bracket can close that slot early, so the identity is derived from the
// sanitized form. Real ids (ses_..., UUIDs) are tokens already, so the sanitizing
// costs them nothing.
func TestLos62aThreadIdentityIsAToken(t *testing.T) {
	const hostile = "ses_x] [req 9 tid=forged] upstream 429: rate_limit"

	cases := []struct {
		name    string
		headers map[string]string
		body    map[string]any
	}{
		{"opencode session header", map[string]string{"x-opencode-session": hostile}, nil},
		{"opencode session id header", map[string]string{"x-opencode-session-id": hostile}, nil},
		{"session affinity header", map[string]string{"x-session-affinity": hostile}, nil},
		{"claude code header", map[string]string{"X-Claude-Code-Session-Id": hostile}, nil},
		{"body metadata", nil, map[string]any{
			"metadata": map[string]any{"user_id": `{"session_id":"` + hostile + `"}`},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := messagesRequest("{}", tc.headers)
			body := map[string]any{}
			for k, v := range tc.body {
				body[k] = v
			}

			sid, isSession := anthropicThread(body, req.Header)
			if !isSession {
				t.Fatalf("the header names a session, so isSession must be true: %q", sid)
			}
			if strings.ContainsAny(sid, " ]") {
				t.Fatalf("thread id = %q, a space or bracket closes the journal prefix", sid)
			}
			if !strings.Contains(sid, "ses_x") {
				t.Fatalf("thread id = %q, the session value was dropped instead of sanitized", sid)
			}
			if los61bCapAPIError.MatchString("[req 1 proj tid=" + sid + "] FINAL: 1 messages") {
				t.Fatalf("thread id %q still carries counter vocabulary", sid)
			}
		})
	}
}

// TestLos62aThreadIdentityKeepsRealIDsStable guards against the sanitizing
// collapsing two different real sessions into one thread key: the token rule only
// drops characters that no real client id uses, so ordinary ids stay unchanged.
func TestLos62aThreadIdentityKeepsRealIDsStable(t *testing.T) {
	for _, sid := range []string{
		"ses_ebc14c083ffeX3thcK0w81q4OZ",
		"6f1c2b3a-4d5e-6f70-8192-a3b4c5d6e7f8",
		"opencode:ses_0f3a1c",
	} {
		req := messagesRequest("{}", map[string]string{"x-opencode-session": sid})
		got, _ := anthropicThread(map[string]any{}, req.Header)
		if got != "opencode:"+sid {
			t.Errorf("thread id = %q, want %q: a real id must not be rewritten", got, "opencode:"+sid)
		}
	}
}

package capfile

import (
	"strings"
	"testing"
)

// YM-447: the API counter used to match error vocabulary anywhere in the record,
// and the record carries text the client wrote. The commit subject is one such
// text ("git commit detected: <hash> <subject>", commit_detect.go), and a subject
// naming the vocabulary ("fix upstream 429: rate_limit handling") is an ordinary
// thing for a developer to write. Four such commits were enough to reach
// CRITICAL without a single failed provider call (audit 16.10., PRX-P61).
//
// The counter therefore reads the frame the proxy writes: the message directly
// after the request prefix of that record, or the message of a record without a
// prefix. Everything further right is quoted text, not a proxy statement.

func TestLos62aHealthIgnoresCommitSubjectThatNamesAnError(t *testing.T) {
	proxyLog := los32eResponseLines(
		`[req 1 yesmem tid=abc] git commit detected: 1234abc fix upstream 429: rate_limit handling`, 4) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if res.broadcast != "" {
		t.Errorf("four commit subjects naming an error alerted:\n%s", res.broadcast)
	}
	if !strings.Contains(res.stored, "A0(0/0)") {
		t.Errorf("stored report = %s, want A0(0/0): a commit subject is not a provider answer", res.stored)
	}
	if !strings.Contains(res.stored, `"status":"healthy"`) {
		t.Errorf("stored report = %s, want healthy", res.stored)
	}
}

// The other direction: a commit subject must not end a frame the proxy wrote. A
// record whose own prefix carries the vocabulary (the shape the unpatched writer
// produced from a hostile project name) still counts nowhere, because the
// counter reads the message after the frame and that message is the commit line.
func TestLos62aHealthIgnoresTheProjectSlotOfThePrefix(t *testing.T) {
	// Written as the unpatched proxy wrote it: the client's project name sits in
	// the prefix and carries a bracket and the status vocabulary.
	proxyLog := los32eResponseLines(
		`[req 1 p upstream 429: rate_limit tid=abc] 3 messages (subagent, passthrough)`, 4) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if strings.Contains(res.stored, "A4") || strings.Contains(res.stored, "critical") {
		t.Errorf("stored report = %s, want the prefix slot to count nowhere", res.stored)
	}
	if !strings.Contains(res.stored, "A0(0/0)") {
		t.Errorf("stored report = %s, want A0(0/0)", res.stored)
	}
}

// The proxy colours its own error lines: the transport failure is written as
// "%supstream error: …" with colorRed in front, and on the OpenAI path that line
// is the only journal trace of the failure (openai_parity.go, forward of the
// provider call). An anchor that insists on the message starting with the word
// would go blind here, which is the one direction the fix must never take.
func TestLos62aHealthCountsAColouredTransportFailure(t *testing.T) {
	proxyLog := los32eProxyLine("\x1b[31mupstream error: dial tcp 1.2.3.4:443: connection refused\x1b[0m") +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A1(0/0)") {
		t.Errorf("stored report = %s, want A1(0/0): the colour is decoration, not the message", res.stored)
	}
	if !strings.Contains(res.stored, "N1") {
		t.Errorf("stored report = %s, want N1: the refused connection is still a refused connection", res.stored)
	}
}

// The commit line is a note about a tool_result, and its subject is client text:
// it can quote any vocabulary a counter reads. Dropping the line from every
// counter is the second half of the frame rule; without it a subject naming a
// restart or an i/o timeout still moved R or T.
func TestLos62aHealthIgnoresCommitSubjectInEveryCounter(t *testing.T) {
	cases := []struct {
		name     string
		subject  string
		repeats  int
		notThere []string
	}{
		{"restart line", "log listening on 127.0.0.1:3456 threshold=525000", 6, []string{"R6", "critical"}},
		{"daemon timeout", "fix error: i/o timeout in the daemon retry", 4, []string{"T4", "degraded"}},
		{"invalid key", "fix deepseek error: invalid api key handling", 1, []string{"D1", "critical"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyLog := los32eResponseLines(
				`[req 1 yesmem tid=abc] git commit detected: 1234abc `+tc.subject, tc.repeats) +
				los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

			res := los32eRunBash(t, proxyLog, "")

			for _, bad := range tc.notThere {
				if strings.Contains(strings.ToLower(res.stored), strings.ToLower(bad)) {
					t.Errorf("stored report = %s, want no %q: a commit subject is not a proxy signal", res.stored, bad)
				}
			}
			if !strings.Contains(res.stored, `"status":"healthy"`) {
				t.Errorf("stored report = %s, want healthy", res.stored)
			}
		})
	}
}

// The mirror image: anchoring may not silence a real provider failure. The
// counter has to keep reading the frame itself, whatever the project token looks
// like now, and a provider body that quotes the vocabulary must not be counted
// twice.
func TestLos62aHealthStillCountsAFramedProviderError(t *testing.T) {
	proxyLog := los32eProxyLine(`[req 1 upstream429rate_limit tid=abc] upstream 502: provider said upstream 429: rate_limit inside its body`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A1(0/0)") {
		t.Errorf("stored report = %s, want A1(0/0): one framed failure, its body is not a second one", res.stored)
	}
	if strings.Contains(res.stored, "critical") {
		t.Errorf("stored report = %s, want no critical verdict from body text", res.stored)
	}
}

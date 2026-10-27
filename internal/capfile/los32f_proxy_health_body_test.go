package capfile

import (
	"strings"
	"testing"
)

// A provider body is foreign text, and the counter rules match by keyword. A
// 500 whose body quotes "upstream 529:", "overload", "rate_limit", "listening
// on ... threshold=" or "[keepalive] ping" used to move every counter of that
// line: false alarms, and a cache total that dilutes a real cache problem.
// Classification reads the status header, not the body.
func TestLos32fHealthIgnoresCounterVocabularyInUpstreamBody(t *testing.T) {
	proxyLog := los32eProxyLine(`[req 1 tid=x] upstream 500: boom upstream 529: overload rate_limit cache: listening on :9099 threshold=0.5 [keepalive] ping tid=y cache_write=9999`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A1(0/0) F0 K0(0) R0 SF0") {
		t.Errorf("the body moved the counters, stored report = %s", res.stored)
	}
	if !strings.Contains(res.stored, "C0/0") {
		t.Errorf("the body forged a cache window, stored report = %s", res.stored)
	}
}

// The thread id sits in the same line as the status and comes from the client,
// so the header part of the record must not include it: a tid of "overload"
// turned an ordinary 502 into a critical overload alarm, and a tid quoting a
// status could push the real one out of the header.
func TestLos32fHealthIgnoresCounterVocabularyInThreadID(t *testing.T) {
	proxyLog := los32eProxyLine(`[req 4 tid=overload rate_limit cache: (5% hit)] upstream 502: realbody`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A1(0/0) F0 K0(0) R0 SF0") {
		t.Errorf("the thread id moved the counters, stored report = %s", res.stored)
	}
	if !strings.Contains(res.stored, "C0/0") {
		t.Errorf("the thread id forged a cache window, stored report = %s", res.stored)
	}
}

// A tid that quotes a status must not hide or invent one: the header is cut at
// the real prefix, and the id itself carries no bracket that could end it early
// (journalToken strips it before the line is written).
func TestLos32fHealthReadsTheRealStatusAfterAForgedOne(t *testing.T) {
	proxyLog := los32eProxyLine(`[req 5 tid=a upstream 529: no] upstream 400: plain error`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A1(0/0)") {
		t.Errorf("stored report = %s, want A1(0/0): the real header says 400", res.stored)
	}
}

// The header keeps its own meaning: a real status line still counts, and the
// body stays in the journal for the operator even though it is not classified.
func TestLos32fHealthStillCountsTheStatusHeader(t *testing.T) {
	proxyLog := los32eProxyLine(`[req 1 tid=x] upstream 429: {"error":{"message":"slow down"}}`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A1(0/1)") {
		t.Errorf("stored report = %s, want A1(0/1): one error, one rate limit", res.stored)
	}
}

package capfile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The live alert on 17.10. read "PROXY HEALTH [CRITICAL] A0(0/94) ... BE462".
// Both numbers were noise: the rate counter matched the substring "429" inside
// a tool_call_id, and BE counted scheduled jobs that legitimately print nothing
// every 15 seconds. A CRITICAL that fires on such text is worse than none,
// because every later alert is read as noise too. These tests run the real bash
// handler with stubbed journalctl/nc/store and pin which log lines may raise an
// alarm: real upstream failures yes, log text that merely looks like a status
// code no.

type los32eResult struct {
	exitCode  int
	stderr    string
	broadcast string
	stored    string
}

// los32eRunBash feeds the handler two canned journal windows. The proxy window
// is what journalctl would print for the proxy unit, the daemon window for the
// daemon unit.
func los32eRunBash(t *testing.T, proxyLog, daemonLog string) los32eResult {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeStub(t, filepath.Join(bin, "journalctl"), `#!/bin/sh
case "$*" in
  *yesmem-proxy*) cat "$LOS32E_PROXY_LOG" ;;
  *) cat "$LOS32E_DAEMON_LOG" ;;
esac
exit 0
`)
	writeStub(t, filepath.Join(bin, "nc"), `#!/bin/sh
cat > "$LOS32E_NC_STDIN"
printf '%s\n' '{"result":{"status":"broadcast_sent"}}'
exit 0
`)

	proxyPath := filepath.Join(dir, "proxy.log")
	daemonPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(proxyPath, []byte(proxyLog), 0o644); err != nil {
		t.Fatalf("write proxy log: %v", err)
	}
	if err := os.WriteFile(daemonPath, []byte(daemonLog), 0o644); err != nil {
		t.Fatalf("write daemon log: %v", err)
	}

	store := filepath.Join(dir, "store.log")
	ncStdin := filepath.Join(dir, "nc.stdin")
	script := filepath.Join(dir, "run.sh")
	body := `store() {
  printf '%s\n' "$1" >> "$LOS32E_STORE"
  return 0
}
` + bundledScript(t, proxyHealthCapPath, "proxy_health_bash")
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"YESMEM_SOCK="+filepath.Join(dir, "job.sock"),
		"YESMEM_PROJECT=/home/tester/memory/yesmem",
		"ALERT=true",
		"HOURS=1",
		"LOS32E_PROXY_LOG="+proxyPath,
		"LOS32E_DAEMON_LOG="+daemonPath,
		"LOS32E_STORE="+store,
		"LOS32E_NC_STDIN="+ncStdin,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := los32eResult{stderr: stderr.String()}
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run handler: %v", err)
		}
		res.exitCode = exitErr.ExitCode()
	}
	if data, err := os.ReadFile(ncStdin); err == nil {
		res.broadcast = string(data)
	}
	if data, err := os.ReadFile(store); err == nil {
		res.stored = string(data)
	}
	return res
}

// los32eProxyLine wraps a payload in the proxy's real log envelope.
func los32eProxyLine(payload string) string {
	return fmt.Sprintf("Oct 17 18:00:00 host yesmem-proxy[42]: 2026/10/17 18:00:00 %s\n", payload)
}

// los32eResponseLines repeats a line often enough that a counter which matches
// it crosses every threshold in the script.
func los32eResponseLines(payload string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(los32eProxyLine(payload))
	}
	return b.String()
}

// The live false positive: a tool_call_id ending in 3429 contains "429", and
// the counter read that as a rate-limit answer from the provider. Request and
// response bookkeeping is not an API error.
func TestLos32eHealthIgnoresStatusDigitsInsideLogText(t *testing.T) {
	proxyLog := los32eResponseLines(
		`OPENAI-FWD: tool msg tool_call_id=call_00_ET_0GMIDZCpgzwRG5WFBTDU3429 content_type=string`, 120) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if res.broadcast != "" {
		t.Errorf("the handler alerted on a tool_call_id that happens to contain 429:\n%s", res.broadcast)
	}
	if !strings.Contains(res.stored, `"status":"healthy"`) {
		t.Errorf("stored report = %s, want healthy: no request failed", res.stored)
	}
}

// The counter must keep working where it matters: a real rate-limit answer is
// an upstream line with status 429 and an error body.
func TestLos32eHealthCountsRealUpstreamRateLimitErrors(t *testing.T) {
	proxyLog := los32eResponseLines(
		`[req 7 tid=abc] upstream 429: {"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`, 4)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.broadcast, "[CRITICAL]") {
		t.Errorf("four real upstream 429 answers produced no CRITICAL alert (stderr=%q)", res.stderr)
	}
	if !strings.Contains(res.broadcast, "A4(0/4)") {
		t.Errorf("broadcast = %s, want A4 with 4 rate-limit hits", res.broadcast)
	}
}

// A single upstream overload answer is still a real failure and stays critical.
// Two shapes count: the 529 status, and an error body that says overloaded.
func TestLos32eHealthCountsRealOverloadErrors(t *testing.T) {
	for name, body := range map[string]string{
		"status 529 without the word": `{"type":"api_error","message":"Service busy"}`,
		"body names the overload":     `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	} {
		proxyLog := los32eProxyLine(`[req 3 tid=abc] upstream 529: ` + body)

		res := los32eRunBash(t, proxyLog, "")

		if !strings.Contains(res.broadcast, "[CRITICAL]") {
			t.Errorf("%s: one real overloaded answer produced no CRITICAL alert (stderr=%q)", name, res.stderr)
		}
	}
}

// The proxy answers a failed keepalive ping with the provider's status and body
// in the same journal. Those are real provider failures and must stay counted;
// the framing change may not silence a path it used to see.
func TestLos32eHealthCountsKeepaliveProviderErrors(t *testing.T) {
	proxyLog := los32eResponseLines(
		`[keepalive] ping error tid=abc remaining=3 mode=anthropic: HTTP 429 body={"type":"error","error":{"type":"rate_limit_error"}}`, 6)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.broadcast, "[CRITICAL]") {
		t.Errorf("six rate-limited keepalive pings produced no CRITICAL alert (stderr=%q)", res.stderr)
	}
	if !strings.Contains(res.broadcast, "A6(0/6)") {
		t.Errorf("broadcast = %s, want A6 with 6 rate-limit hits", res.broadcast)
	}
}

// A fork's provider call logs its transport failure on its own line.
func TestLos32eHealthCountsForkProviderErrors(t *testing.T) {
	proxyLog := los32eResponseLines(
		`[req 3 v2] fork learning: API error: Post "https://api.example/v1/messages": dial tcp 10.0.0.1:443: connection refused`, 11)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.broadcast, "N11") {
		t.Errorf("broadcast = %q, want N11: eleven refused fork connections", res.broadcast)
	}
	if !strings.Contains(res.broadcast, "[CRITICAL]") {
		t.Errorf("eleven refused connections produced no CRITICAL alert (stderr=%q)", res.stderr)
	}
}

// The other side of anchoring restarts: the real startup line still counts,
// otherwise a restart storm would pass unseen.
func TestLos32eHealthCountsRestartLines(t *testing.T) {
	proxyLines := los32eResponseLines(
		`listening on 127.0.0.1:8787 → https://api.example (threshold=500000, keepRecent=3, model_thresholds=map[])`, 6)

	res := los32eRunBash(t, proxyLines, "")

	if !strings.Contains(res.broadcast, "R6") {
		t.Errorf("broadcast = %q, want R6: six real restarts", res.broadcast)
	}
	if !strings.Contains(res.broadcast, "[CRITICAL]") {
		t.Errorf("six restarts produced no CRITICAL alert (stderr=%q)", res.stderr)
	}
}

// Two scheduled jobs that print nothing every 15 seconds are how "OK (0 bytes)"
// looks by design. Counting them put a four-digit number of "findings" into the
// alert line while nothing was wrong.
func TestLos32eHealthIgnoresEmptyScheduledRuns(t *testing.T) {
	var daemonLog strings.Builder
	for i := 0; i < 231; i++ {
		daemonLog.WriteString("Oct 17 18:00:00 host yesmem[42]: 2026/10/17 18:00:00 [scheduler] bash job sched-rocketchat-poll-74211 OK (0 bytes)\n")
		daemonLog.WriteString("Oct 17 18:00:00 host yesmem[42]: 2026/10/17 18:00:00 [scheduler] bash job sched-rocketchat-reply-74526 OK (0 bytes)\n")
	}
	proxyLog := los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, daemonLog.String())

	if !strings.Contains(res.stored, `"status":"healthy"`) {
		t.Fatalf("stored report = %q, want a healthy report: every run succeeded", res.stored)
	}
	if strings.Contains(res.stored, "BE") {
		t.Errorf("stored report = %s, want no tally of empty-but-successful runs", res.stored)
	}
	if strings.Contains(res.stored, "critical") || strings.Contains(res.stored, "degraded") {
		t.Errorf("stored report = %s, want healthy: every run succeeded", res.stored)
	}
	if res.broadcast != "" {
		t.Errorf("the handler alerted although every run succeeded:\n%s", res.broadcast)
	}
}

// A request that never reached the provider is logged as "upstream error: ...".
// The bare words without the colon are not that line, and the counter has to
// follow the log, not the other way round.
func TestLos32eHealthCountsTransportFailures(t *testing.T) {
	proxyLog := los32eProxyLine(`upstream error: dial tcp 1.2.3.4:443: connection refused`) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A1(0/0)") {
		t.Errorf("stored report = %s, want the transport failure counted in A", res.stored)
	}
	if !strings.Contains(res.stored, "N1") {
		t.Errorf("stored report = %s, want the refused connection counted in N", res.stored)
	}
}

// The colon is what makes the line an error line. Without it this is prose.
func TestLos32eHealthIgnoresProseThatNamesAnUpstreamError(t *testing.T) {
	proxyLog := los32eResponseLines(`upstream error overload in the preview`, 6) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A0(0/0)") {
		t.Errorf("stored report = %s, want A0(0/0): no API error from prose", res.stored)
	}
	if res.broadcast != "" {
		t.Errorf("the handler alerted on prose:\n%s", res.broadcast)
	}
}

// Fork previews quote model output back into the proxy journal. Six of them
// naming a restart and a timeout used to read as six restarts and six daemon
// timeouts, which is a CRITICAL from text the model wrote.
func TestLos32eHealthIgnoresForkPreviewText(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteString(los32eProxyLine(`[fork] extract_and_evaluate: parse error (unexpected), response preview: the proxy was listening on :8787 when the i/o timeout hit`))
	}
	proxyLog := b.String() + los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if res.broadcast != "" {
		t.Errorf("the handler alerted on a fork preview:\n%s", res.broadcast)
	}
	if !strings.Contains(res.stored, `"status":"healthy"`) {
		t.Errorf("stored report = %s, want healthy: no restart and no timeout happened", res.stored)
	}
}

// A thread id can contain any digits. Inside a framed line the status has to be
// read where the proxy writes it, not as a bare number, so a status digit in a
// thread called ses_x529y is an API error and not an overload. The sample is the
// provider-failure line the proxy writes; a deadline is no longer one of them
// (it has its own line since YM-347).
func TestLos32eHealthIgnoresStatusDigitsInsideAThreadID(t *testing.T) {
	proxyLog := los32eProxyLine(`[keepalive] ping error tid=opencode:ses_x529y ctx_tokens=1200 remaining=3 mode=5m: HTTP 503 body={"type":"error","error":{"type":"api_error"}}`)

	res := los32eRunBash(t, proxyLog, "")

	if strings.Contains(res.broadcast, "[CRITICAL]") {
		t.Errorf("the handler went critical on a thread id containing 529:\n%s", res.broadcast)
	}
	if !strings.Contains(res.stored, "A1(0/0)") {
		t.Errorf("stored report = %s, want A1(0/0): one error, no overload, no rate limit", res.stored)
	}
}

// A daemon timeout is a real error and has to keep counting: the guard that
// excludes model text may not silence the shape it was meant to keep. The
// proxy labels its daemon calls differently per site, so every real label is
// pinned here.
func TestLos32eHealthCountsDaemonTimeouts(t *testing.T) {
	for name, line := range map[string]string{
		"daemon error":       `doc-context: daemon error: query hybrid_search: read unix /run/yesmem.sock: i/o timeout`,
		"coding-query error": `doc-context: coding-query search error: read unix /run/yesmem.sock: i/o timeout`,
		"read body error":    `[req 5] read body error: read unix /run/yesmem.sock: i/o timeout`,
		"body read error":    `[req 5 tid=x] read error: read unix /run/yesmem.sock: i/o timeout`,
		"write error":        `[req 5] write error: write unix /run/yesmem.sock: i/o timeout`,
	} {
		proxyLog := los32eResponseLines(line, 4) + los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=2 out=3 remaining=1 mode=5m`)

		res := los32eRunBash(t, proxyLog, "")

		if !strings.Contains(res.stored, "T4") {
			t.Errorf("%s: stored report = %s, want T4: four daemon timeouts", name, res.stored)
		}
		if !strings.Contains(res.stored, `"status":"degraded"`) {
			t.Errorf("%s: stored report = %s, want degraded after four daemon timeouts", name, res.stored)
		}
	}
}

// K is the number of pings that actually warmed the cache. A ping the upstream
// rejected is an API error and must not be reported as a ping.
func TestLos32eHealthCountsOnlySuccessfulPings(t *testing.T) {
	proxyLog := los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`) +
		los32eProxyLine(`[keepalive] ping error tid=abc ctx_tokens=900 remaining=0 mode=5m: HTTP 503 body={"type":"error","error":{"type":"api_error"}}`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "K1(0)") {
		t.Errorf("stored report = %s, want K1(0): one successful ping, one failed attempt", res.stored)
	}
}

// An invalid provider key is a real failure and stays critical on its own.
func TestLos32eHealthCountsProviderAuthFailures(t *testing.T) {
	proxyLog := los32eProxyLine(`[req 9 tid=abc] upstream 401: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.broadcast, "D1") {
		t.Errorf("broadcast = %q, want D1: one auth failure", res.broadcast)
	}
	if !strings.Contains(res.broadcast, "[CRITICAL]") {
		t.Errorf("an invalid api key produced no CRITICAL alert (stderr=%q)", res.stderr)
	}
}

// An upstream read timeout is a provider failure, not a daemon one: it belongs
// in A/N and must not push the daemon-timeout counter.
func TestLos32eHealthSeparatesUpstreamFromDaemonTimeouts(t *testing.T) {
	proxyLog := los32eResponseLines(`upstream error: read tcp 10.0.0.1:443: i/o timeout`, 4) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A4(0/0)") {
		t.Errorf("stored report = %s, want A4: four upstream failures", res.stored)
	}
	if !strings.Contains(res.stored, "T0") {
		t.Errorf("stored report = %s, want T0: they are not daemon timeouts", res.stored)
	}
}

// The fork preview is the exact line that carries model output into the
// journal, and that output can quote the error frames themselves.
func TestLos32eHealthIgnoresFrameWordsInsideAForkPreview(t *testing.T) {
	preview := `[fork] extract_and_evaluate: parse error (invalid character), response preview: upstream 429: rate_limit, listening on 0.0.0.0 (threshold=1), daemon error: i/o timeout`
	proxyLog := los32eResponseLines(preview, 4) +
		los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, "")

	if strings.Contains(res.broadcast, "[CRITICAL]") {
		t.Errorf("the handler went critical on model text quoted into a fork preview:\n%s", res.broadcast)
	}
	if !strings.Contains(res.stored, "A0(0/0)") {
		t.Errorf("stored report = %s, want A0(0/0): a preview is not an API error", res.stored)
	}
	if !strings.Contains(res.stored, "F4") {
		t.Errorf("stored report = %s, want F4: the four fork diagnostics still count as fork activity", res.stored)
	}
}

// The mirror image of the false alarm: a real scheduler failure was never
// counted, because the log writes "FAILED" and the pattern asked for "failed".
func TestLos32eHealthCountsFailedScheduledRuns(t *testing.T) {
	var daemonLog strings.Builder
	// The real window mixes both: hundreds of successful silent runs and one
	// failure. Only the failure may raise the alarm.
	for i := 0; i < 231; i++ {
		daemonLog.WriteString("Oct 17 18:00:00 host yesmem[42]: 2026/10/17 18:00:00 [scheduler] bash job sched-rocketchat-poll-74211 OK (0 bytes)\n")
		daemonLog.WriteString("Oct 17 18:00:00 host yesmem[42]: 2026/10/17 18:00:00 [scheduler] bash job sched-rocketchat-reply-74526 OK (0 bytes)\n")
	}
	daemonLog.WriteString("Oct 17 18:00:00 host yesmem[42]: 2026/10/17 18:00:00 [scheduler] bash job sched-rocketchat-poll-74211 FAILED (exit=1): exit status 1\n")
	proxyLog := los32eProxyLine(`[keepalive] ping tid=abc cache_read=1 cache_write=42 out=3 remaining=1 mode=5m`)

	res := los32eRunBash(t, proxyLog, daemonLog.String())

	if !strings.Contains(res.broadcast, "SF1") {
		t.Errorf("a real scheduler failure was not counted (broadcast=%q stderr=%q)", res.broadcast, res.stderr)
	}
	if !strings.Contains(res.stored, `"status":"degraded"`) {
		t.Errorf("stored report = %s, want degraded after a failed job", res.stored)
	}
}

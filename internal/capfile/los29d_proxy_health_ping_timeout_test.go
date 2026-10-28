package capfile

import (
	"fmt"
	"strings"
	"testing"
)

// los29dTimeoutLine is the line the proxy writes when a ping runs into its own
// budget (internal/proxy/cache_keepalive.go, YM-347).
func los29dTimeoutLine(tokens, remaining int) string {
	return los32eProxyLine(fmt.Sprintf(`[keepalive] ping timeout tid=opencode:ses_big ctx_tokens=%d body_bytes=2960000 timeout=1m44s remaining=%d mode=auto: Post "https://api.anthropic.com/v1/messages": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`, tokens, remaining))
}

// A keepalive ping that ran into its own budget is a large-context signal, not
// an upstream failure. Read as an API error it pushed a 750k-token session to
// "degraded: 8 API err" with no upstream error anywhere in the journal, which is
// exactly the misleading report the ticket was filed about.
func TestLos29dHealthCountsPingTimeoutsApart(t *testing.T) {
	proxyLog := los29dTimeoutLine(740000, 3) + los29dTimeoutLine(690000, 2) + los29dTimeoutLine(655000, 1)

	res := los38nRunHandler(t, proxyLog, "", map[string]string{
		"ALERT":           "true",
		"YESMEM_PROJECT":  "/home/tester/memory/yesmem",
		"LOS38N_NC_REPLY": `{"result":{}}`,
	})

	if !strings.Contains(res.report, `"status":"degraded"`) {
		t.Fatalf("report = %s, want degraded: the cache of that session is not kept warm", res.report)
	}
	if !strings.Contains(res.report, "K0(0/3)") {
		t.Errorf("report = %s, want K0(0/3): zero warmed pings, three timeouts", res.report)
	}
	if !strings.Contains(res.report, "A0(0/0)") {
		t.Errorf("report = %s, want A0(0/0): a timeout is not an upstream error", res.report)
	}
	// The K token carries only the count, so the alert has to name the cause:
	// "K0(0/3)" alone reads like an outage.
	if !strings.Contains(res.broadcast, "prompt exceeded the ping budget") {
		t.Errorf("broadcast = %s, want the timeout named as a budget problem", res.broadcast)
	}
	if !strings.Contains(res.broadcast, "largest context 740000 tokens") {
		t.Errorf("broadcast = %s, want the largest context size of the window", res.broadcast)
	}
	// The stored report carries the same sentence: the broadcast is gone in a
	// minute, the report is what an operator reads later.
	if !strings.Contains(res.report, "prompt exceeded the ping budget") {
		t.Errorf("report = %s, want the cause stored, not only announced", res.report)
	}
	if strings.Contains(res.broadcast, "API errors") {
		t.Errorf("broadcast = %s, want no API error section: no upstream error happened", res.broadcast)
	}
}

// One timeout in a long window is a session that ended, not a condition: the
// report stays healthy, the way it tolerates up to five API errors. A standing
// degraded verdict is what the ticket complained about.
func TestLos29dHealthToleratesASinglePingTimeout(t *testing.T) {
	proxyLog := los29dTimeoutLine(740000, 2) +
		los32eProxyLine(`[keepalive] ping tid=other cache_read=500000 cache_write=120 out=4 remaining=2 mode=auto`)

	res := los38nRunHandler(t, proxyLog, "", nil)

	if !strings.Contains(res.report, `"status":"healthy"`) {
		t.Errorf("report = %s, want healthy: one timeout is not a standing condition", res.report)
	}
	if !strings.Contains(res.report, "K1(0/1)") {
		t.Errorf("report = %s, want K1(0/1): one warmed ping and one timeout", res.report)
	}
}

// The other direction has to stay: a ping the upstream rejected is an upstream
// answer, keeps its own counter and must not be folded into the timeouts.
func TestLos29dHealthKeepsUpstreamPingFailures(t *testing.T) {
	upstream := los32eProxyLine(`[keepalive] ping error tid=abc ctx_tokens=2000 remaining=3 mode=5m: HTTP 503 body={"type":"error","error":{"type":"api_error"}}`)

	res := los38nRunHandler(t, upstream, "", nil)

	if !strings.Contains(res.report, "A1(0/0)") {
		t.Errorf("report = %s, want A1(0/0): the rejected ping is an API error, no overload, no rate limit", res.report)
	}
	if strings.Contains(res.report, "K0(0/") {
		t.Errorf("report = %s, want no timeout suffix for a rejected ping", res.report)
	}
}

// A window can hold both: a warmed ping and a later timeout. The K token stays
// in its established form and grows the timeout field only where one exists.
func TestLos29dHealthMixedWindowKeepsTheKTokenReadable(t *testing.T) {
	proxyLog := los32eProxyLine(`[keepalive] ping tid=abc cache_read=500000 cache_write=120 out=4 remaining=2 mode=auto`) +
		los29dTimeoutLine(760000, 1)

	res := los38nRunHandler(t, proxyLog, "", nil)

	if !strings.Contains(res.report, "K1(0/1)") {
		t.Errorf("report = %s, want K1(0/1): one warmed ping and one timeout", res.report)
	}
	if !strings.Contains(res.report, "A0(0/0)") {
		t.Errorf("report = %s, want A0(0/0)", res.report)
	}
}

// The split has to exist in the JS variant too. Reading the two branches apart
// is half the fix if the other variant keeps counting timeouts as API errors,
// so the source is pinned at the grep statements themselves, which is what runs,
// not at a comment that merely mentions the pattern.
func TestLos29dBundledCapSeparatesPingTimeoutInBothVariants(t *testing.T) {
	jsBody := bundledScript(t, proxyHealthCapPath, "proxy_health")

	if !strings.Contains(jsBody, "timeouts: kaTimeoutLines.length") {
		t.Error("the JS variant does not report the timeouts as their own field")
	}

	var apiGrep, timeoutGrep bool
	for _, line := range strings.Split(jsBody, "\n") {
		if !strings.Contains(line, "sh(") {
			continue
		}
		if strings.Contains(line, "ping timeout tid=") {
			timeoutGrep = true
		}
		if strings.Contains(line, "ping error tid=") {
			apiGrep = true
			if strings.Contains(line, "ping timeout tid=") {
				t.Errorf("the same grep reads both lines: %s", strings.TrimSpace(line))
			}
		}
	}
	if !timeoutGrep {
		t.Error("the JS variant has no grep for the timeout line: a large-context session would look like an outage there")
	}
	if !apiGrep {
		t.Error("the JS variant lost its grep for real upstream ping failures")
	}
	// The tolerance is what keeps a single timeout from holding the report at
	// degraded for a whole 1..168h window, and a quiet window must not degrade on
	// its own. Both rules live in source only, so they are pinned here.
	if !strings.Contains(jsBody, "keepalive.timeouts > 2") {
		t.Error("the JS variant has no three-timeout tolerance")
	}
	if !strings.Contains(jsBody, "keepalive.total === 0 && otherSignals > 0") {
		t.Error("the JS variant degrades a quiet window: it lacks the otherSignals guard the bash handler has")
	}
}

package capfile

import (
	"fmt"
	"strings"
	"testing"
)

// finding 3 of the audit: SCHED_FAIL matched only "scheduler.*bash job.*FAILED".
// A headless job fails with its own wording ("failed:", "timed out"), so a
// scheduler whose only failing work is headless read as healthy and the alert
// never fired. The lines below are the ones the daemon writes
// (internal/daemon/handler_scheduler.go executeHeadless).
func TestLos32fHealthCountsHeadlessJobFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		daemonLog string
		wantSF    string
	}{
		"failed run": {
			daemonLog: los32fDaemonLine(`[scheduler] headless job sched-research-9182 failed: exit status 1 (stderr: boom)`),
			wantSF:    "SF1",
		},
		"timed out run": {
			daemonLog: los32fDaemonLine(`[scheduler] headless job sched-research-9182 timed out after 10m0s`),
			wantSF:    "SF1",
		},
		"two failing jobs": {
			daemonLog: los32fDaemonLine(`[scheduler] headless job sched-research-9182 failed: exit status 1 (stderr: boom)`) +
				los32fDaemonLine(`[scheduler] headless job sched-refine-4410 timed out after 10m0s`),
			wantSF: "SF2",
		},
		"bash job failure still counted": {
			daemonLog: los32fDaemonLine(`[scheduler] bash job sched-rocketchat-poll-74211 FAILED (exit=1): exit status 1`),
			wantSF:    "SF1",
		},
		// executeHeadless writes this form when the run never starts, e.g. a
		// sandbox profile whose ai-jail is missing. It is a failure, not a skip,
		// and it used to carry no "failed:" at all.
		"run refused before start": {
			daemonLog: los32fDaemonLine(`[scheduler] headless job sched-research-9182: exec: "ai-jail": executable file not found in $PATH`),
			wantSF:    "SF1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			res := los32eRunBash(t, "", tc.daemonLog)

			if !strings.Contains(res.stored, `"status":"degraded"`) {
				t.Errorf("stored report = %s, want degraded: a scheduled job failed", res.stored)
			}
			if !strings.Contains(res.stored, tc.wantSF+" C") {
				t.Errorf("stored report = %s, want %s in the summary", res.stored, tc.wantSF)
			}
		})
	}
}

// The other half of the rule: a scheduler that ran headless jobs normally, or
// skipped one for lack of a work dir, is not failing. Counting those would
// turn every quiet window into a degraded verdict (the YM-289 noise pattern).
func TestLos32fHealthIgnoresSuccessfulAndSkippedHeadlessRuns(t *testing.T) {
	daemonLog := los32fDaemonLine(`[scheduler] headless run for job sched-research-9182 (resume=false, sandbox=standard)`) +
		los32fDaemonLine(`[scheduler] headless job sched-research-9182 done (412 bytes, session=ses_ab12, section=sched-research, under "/home/tester/memory/yesmem")`) +
		los32fDaemonLine(`[scheduler] headless job sched-refine-4410 skipped: no working directory`)

	res := los32eRunBash(t, "", daemonLog)

	if strings.Contains(res.stored, `"status":"degraded"`) {
		t.Errorf("stored report = %s, want healthy: no headless run failed", res.stored)
	}
	if !strings.Contains(res.stored, "SF0 C") {
		t.Errorf("stored report = %s, want SF0", res.stored)
	}
}

// A read error on the upstream body is a provider failure, not a daemon
// timeout. The two counters are read from the same journal window, so the
// wording decides the bucket: "upstream error:" wins, a bare "read error:" was
// counted as T because of its "i/o timeout".
func TestLos32fHealthClassifiesUpstreamReadErrorAsAPIError(t *testing.T) {
	proxyLog := los32eProxyLine(`[req 1 tid=s1] upstream error: read from upstream: read tcp 10.0.0.1:443: i/o timeout`)

	res := los32eRunBash(t, proxyLog, "")

	if !strings.Contains(res.stored, "A1(") {
		t.Errorf("stored report = %s, want A1: the upstream body read failed", res.stored)
	}
	if !strings.Contains(res.stored, "T0") {
		t.Errorf("stored report = %s, want T0: the daemon did not time out", res.stored)
	}
}

// The rules may not invent a failure out of a path or a stderr quote, and one
// line may not count twice: a failed run whose stderr says "timed out after"
// is still one failure.
func TestLos32fHeadlessRulesDoNotOverMatch(t *testing.T) {
	t.Run("success line with failure words in the path", func(t *testing.T) {
		daemonLog := los32fDaemonLine(`[scheduler] headless job sched-x done (12 bytes, session=ses_1, section=sched-x, under "/p/ failed: weird")`)

		res := los32eRunBash(t, "", daemonLog)

		if !strings.Contains(res.stored, "SF0 C") {
			t.Errorf("stored report = %s, want SF0: the run finished", res.stored)
		}
	})

	t.Run("failed run whose stderr quotes the timeout wording", func(t *testing.T) {
		daemonLog := los32fDaemonLine(`[scheduler] headless job sched-x failed: exit status 1 (stderr: timed out after 10m0s)`)

		res := los32eRunBash(t, "", daemonLog)

		if !strings.Contains(res.stored, "SF1 C") {
			t.Errorf("stored report = %s, want SF1: one line, one failure", res.stored)
		}
	})
}

// los32fDaemonLine wraps a payload in the daemon's real journal envelope, the
// form journalctl --user -u yesmem prints.
func los32fDaemonLine(payload string) string {
	return fmt.Sprintf("Oct 17 18:00:00 host yesmem[42]: 2026/10/17 18:00:00 %s\n", payload)
}

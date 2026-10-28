package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFormatSendTo_ReportsTheFullTarget(t *testing.T) {
	raw := json.RawMessage(`{"message_id":7,"target":"opencode:ses_abcdefghijklmnopqrstuvwx"}`)
	got := formatSendTo(raw, nil)
	if !strings.Contains(got, "opencode:ses_abcdefghijklmnopqrstuvwx") {
		t.Errorf("target must be reported in full, got %q", got)
	}
	if !strings.Contains(got, "#7") {
		t.Errorf("message id missing in %q", got)
	}
}

func TestFormatSendTo_AckDroppedIsNotReportedAsSent(t *testing.T) {
	raw := json.RawMessage(`{"message_id":0,"target":"opencode:ses_abcdefghijklmnopqrstuvwx","ack_dropped":true}`)
	got := formatSendTo(raw, nil)
	if strings.Contains(got, "✓") {
		t.Errorf("a dropped ACK must not carry a success marker: %q", got)
	}
	if !strings.Contains(got, "ACK dropped") {
		t.Errorf("dropped ACK must be named: %q", got)
	}
}

// TestFormatSendTo_UnboundTargetIsNotReportedAsSent: the daemon warns when the
// addressed opencode agent has no session yet. Its message is stored under an
// address nothing polls, so the caller must not read the reply as a delivery.
func TestFormatSendTo_UnboundTargetIsNotReportedAsSent(t *testing.T) {
	raw := json.RawMessage(`{"message_id":42,"target":"opencode:ses_abcdefghijklmnopqrstuvwx","warning":"target not bound to a backend session; delivery waits for the bind"}`)
	got := formatSendTo(raw, nil)
	if strings.Contains(got, "✓") {
		t.Errorf("an undeliverable send must not carry a success marker: %q", got)
	}
	if !strings.Contains(got, "delivery waits for the bind") {
		t.Errorf("the daemon's warning must reach the caller: %q", got)
	}
	if !strings.Contains(got, "#42") {
		t.Errorf("message id missing in %q", got)
	}
}

// TestFormatSendTo_APIDeliveryIsNotAdvertisedAsPolling: once the daemon has
// pushed the message into the target's inbox, the target reacts without the
// caller polling for it — advising a poll would undo the point of the push.
func TestFormatSendTo_APIDeliveryNeedsNoPolling(t *testing.T) {
	raw := json.RawMessage(`{"message_id":9,"target":"opencode:ses_abcdefghijklmnopqrstuvwx","delivered_via":"api"}`)
	got := formatSendTo(raw, nil)
	if !strings.Contains(got, "✓") {
		t.Errorf("an active delivery is a success: %q", got)
	}
	if !strings.Contains(got, "#9") || !strings.Contains(got, "opencode:ses_abcdefghijklmnopqrstuvwx") {
		t.Errorf("message id and target must be reported in full, got %q", got)
	}
	if strings.Contains(got, "polling") {
		t.Errorf("an API delivery must not tell the caller to poll: %q", got)
	}
}

// TestFormatSendTo_CronCreateAdviceOnlyForClientsThatHaveIt: the stored-path
// hint named CronCreate, a Claude Code tool. An opencode or codex session has no
// such tool, so the advice pointed at something it cannot do (YM-73).
func TestFormatSendTo_CronCreateAdviceOnlyForClientsThatHaveIt(t *testing.T) {
	raw := json.RawMessage(`{"message_id":13,"target":"opencode:ses_abcdefghijklmnopqrstuvwx","delivered_via":"stored"}`)

	for _, backend := range []string{"opencode", "codex"} {
		got := formatSendTo(raw, map[string]any{"_source_agent": backend})
		if strings.Contains(got, "CronCreate") {
			t.Errorf("%s has no CronCreate tool, the hint must not be offered: %q", backend, got)
		}
		if !strings.Contains(got, "next request") {
			t.Errorf("%s must still learn how the message arrives: %q", backend, got)
		}
	}

	got := formatSendTo(raw, nil)
	if !strings.Contains(got, "CronCreate") {
		t.Errorf("a Claude Code caller keeps the polling hint: %q", got)
	}
}

// TestFormatSendTo_StoredDeliverySaysSo: the caller has to be able to tell that
// the message is waiting for the target's own next request.
func TestFormatSendTo_StoredDeliverySaysSo(t *testing.T) {
	raw := json.RawMessage(`{"message_id":11,"target":"opencode:ses_abcdefghijklmnopqrstuvwx","delivered_via":"stored"}`)
	got := formatSendTo(raw, nil)
	if !strings.Contains(got, "stored") {
		t.Errorf("a stored message must be named as stored: %q", got)
	}
	if !strings.Contains(got, "polling") {
		t.Errorf("a stored message may still need polling: %q", got)
	}
}

// TestFormatSendTo_UnconfirmedMustNotClaimSuccessOrAdviseAResend: the daemon
// could neither confirm nor rule out the push. Saying "sent" would be a
// delivery claim nobody can back, and advising a resend would invite the
// duplicate the claim exists to prevent.
func TestFormatSendTo_UnconfirmedMustNotClaimSuccessOrAdviseAResend(t *testing.T) {
	raw := json.RawMessage(`{"message_id":12,"target":"opencode:ses_abcdefghijklmnopqrstuvwx","delivered_via":"unconfirmed"}`)
	got := formatSendTo(raw, nil)
	if strings.Contains(got, "✓") {
		t.Errorf("an unconfirmed delivery must not carry a success marker: %q", got)
	}
	if !strings.Contains(got, "#12") || !strings.Contains(got, "could not be confirmed") {
		t.Errorf("the caller must learn what is known, got %q", got)
	}
	if !strings.Contains(got, "do not resend") {
		t.Errorf("an unconfirmed turn must not invite a resend: %q", got)
	}
}

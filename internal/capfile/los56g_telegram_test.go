package capfile

import (
	"strings"
	"testing"
)

// YM-377 (OPS-94): four blind spots in the bundled telegram cap, all four measured
// on the old body. They share one cause: an assignment whose status the script
// never sees, because the daemon prepends set -e on the execute_cap path
// (handler_cap_exec.go, fireCapHandler). Every "||" below is the fix that makes
// the following branch reachable.

// (1) telegram_reply assigned the llm result and read its status on the next
// line. Under set -e the run ended at the assignment, so "reply: llm failed" was
// dead code and a model failure counted as a failed job with no log line.
func TestLos56gTelegramReplySurvivesAFailedModelCall(t *testing.T) {
	res := los38xRun(t, "telegram_reply", map[string]string{
		"LOS38X_LLM_EXIT": "3",
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d after a failing llm call, want 0; the run ended before it logged anything (stderr=%q, log=%q)",
			res.exitCode, res.stderr, res.log)
	}
	if !strings.Contains(res.log, "reply: llm failed exit=3") {
		t.Errorf("log = %q, want the failed-llm line the branch writes", res.log)
	}
}

// (2) telegram_poll read the update count unguarded. A 200 answer that is not
// JSON (a proxy error page) made `yesmem json` exit non-zero, so the run ended at
// the assignment and "poll: no messages" was dead code.
func TestLos56gTelegramPollSurvivesAnUnreadableCount(t *testing.T) {
	res := los38xRun(t, "telegram_poll", map[string]string{
		"LOS38X_JSON_FAIL": ".result | length",
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d for a body without a result field, want 0 (stderr=%q, log=%q)",
			res.exitCode, res.stderr, res.log)
	}
	// Not "no messages": a broken parse and an idle bot are different states, and
	// reporting one as the other hides a permanently failing poll.
	if !strings.Contains(res.log, "poll: response unreadable") {
		t.Errorf("log = %q, want the unreadable-response line", res.log)
	}
	if strings.Contains(res.log, "poll: no messages") {
		t.Errorf("log = %q, an unreadable body was reported as an empty result", res.log)
	}
}

// (3) The same sink guarded the per-update reads. An update whose update_id
// cannot be read ended the whole loop, so the offset was never written and the
// batch came again on every run.
func TestLos56gTelegramPollSurvivesAnUnreadableUpdateId(t *testing.T) {
	res := los38xRun(t, "telegram_poll", map[string]string{
		"LOS38X_COUNT":     "1",
		"LOS38X_UPDATE":    `{"update_id":7,"message":{"chat":{"id":42},"from":{"first_name":"Jan"},"text":"hi","date":1}}`,
		"LOS38X_FIRSTNAME": "Jan",
		"LOS38X_JSON_FAIL": ".update_id",
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d for an unreadable update_id, want 0 (stderr=%q, log=%q)",
			res.exitCode, res.stderr, res.log)
	}
	if !strings.Contains(res.log, "poll: done, offset=0") {
		t.Errorf("log = %q, want the run to finish and leave the offset where it was; an unread update_id must not advance it", res.log)
	}
}

// (4) The command branches logged session-table values untouched. A newline in
// one of them writes a line the reader takes for ours, exactly as a Telegram
// message did before YM-368.
func TestLos56gTelegramReplyNeutralisesStoredSessionName(t *testing.T) {
	const injected = "Okt 18 15:00:00 host yesmem[1]: INJECTED-SESSION"
	res := los38xRun(t, "telegram_reply", map[string]string{
		"LOS38X_CLAIM_TEXT": "/status",
		"LOS38X_ROW_NAME":   "default\n" + injected,
		"LOS38X_CURL_BODY":  `{"ok":true,"result":{"message_id":9}}`,
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d for /status (stderr=%q, log=%q)", res.exitCode, res.stderr, res.log)
	}
	if !strings.Contains(res.log, "reply: /status -> ") {
		t.Fatalf("log = %q, want the /status line the stored name lands in", res.log)
	}
	for _, line := range strings.Split(res.log, "\n") {
		if !strings.Contains(line, "INJECTED-SESSION") {
			continue
		}
		if !strings.Contains(line, "reply: /status -> ") {
			t.Errorf("a stored session name wrote its own log line: %q", line)
		}
	}
	if !strings.Contains(res.log, "/status -> default Okt 18") {
		t.Errorf("log = %q, want the stored name kept on the line it belongs to", res.log)
	}
}

// (2b) and (3b) from the cold review: two more assignments of the same class one
// line away from the ones this loss closed. The offset upsert reopens exactly the
// loop the poll ticket closes, and the /models branch reads a config file that a
// user can leave broken — the message is already claimed at that point, so an
// abort swallows it.
func TestLos56gTelegramPollSurvivesAnUnwritableOffset(t *testing.T) {
	res := los38xRun(t, "telegram_poll", map[string]string{
		"LOS38X_COUNT":     "1",
		"LOS38X_UPDATE":    `{"update_id":7,"message":{"chat":{"id":42},"from":{"first_name":"Jan"},"text":"hi","date":1}}`,
		"LOS38X_FIRSTNAME": "Jan",
		"LOS38X_JSON_FAIL": "id:12",
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d for an unwritable offset, want 0 (stderr=%q, log=%q)", res.exitCode, res.stderr, res.log)
	}
	if !strings.Contains(res.log, "poll: offset not stored") {
		t.Errorf("log = %q, want the offset line so the next run's repeat is explainable", res.log)
	}
	if !strings.Contains(res.log, "stored update_id=7") {
		t.Errorf("log = %q, want the update stored before the offset write", res.log)
	}
}

func TestLos56gTelegramReplyAnswersUnreadableOpencodeConfig(t *testing.T) {
	res := los38xRunWithFiles(t, "telegram_reply", map[string]string{
		"LOS38X_CLAIM_TEXT": "/models",
		"LOS38X_JSON_FAIL":  "to_entries",
		"LOS38X_CURL_BODY":  `{"ok":true,"result":{"message_id":9}}`,
	}, map[string]string{
		".config/opencode/opencode.json": "{ this is not json",
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d for a broken opencode.json, want 0; the message was already claimed (stderr=%q, log=%q)",
			res.exitCode, res.stderr, res.log)
	}
	if !strings.Contains(res.log, "reply: /models") {
		t.Errorf("log = %q, want the /models line", res.log)
	}
	if !strings.Contains(res.curl, "sendMessage") {
		t.Errorf("curl = %q, want the broken config answered instead of swallowed", res.curl)
	}
}

// A batch with one unreadable update must stop before the offset can pass it.
// With "continue" the next readable update advanced MAX_ID past the failed one, and
// getUpdates(offset=MAX_ID) never returns it again: the message was lost for good.
func TestLos56gTelegramPollStopsTheBatchAtAnUnreadableUpdate(t *testing.T) {
	res := los38xRun(t, "telegram_poll", map[string]string{
		"LOS38X_COUNT":           "2",
		"LOS38X_UPDATE":          `{"update_id":5,"bad":"los56g-marker","message":{"chat":{"id":42},"from":{"first_name":"Ann"},"text":"first","date":1}}`,
		"LOS38X_UPDATE2":         `{"update_id":7,"message":{"chat":{"id":42},"from":{"first_name":"Ben"},"text":"second","date":1}}`,
		"LOS38X_FIRSTNAME":       "Ann",
		"LOS38X_JSON_FAIL_INPUT": "los56g-marker",
	})

	if res.exitCode != 0 {
		t.Fatalf("exit code %d for a batch with one unreadable update (stderr=%q, log=%q)", res.exitCode, res.stderr, res.log)
	}
	if !strings.Contains(res.log, "batch stopped") {
		t.Errorf("log = %q, want the batch-stopped line", res.log)
	}
	if !strings.Contains(res.log, "poll: done, offset=0") {
		t.Errorf("log = %q, want the offset left before the unreadable update; it must not advance past it", res.log)
	}
	if strings.Contains(res.log, "stored update_id=7") {
		t.Errorf("log = %q, the batch ran past the unreadable update", res.log)
	}
}

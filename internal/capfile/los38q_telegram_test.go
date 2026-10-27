package capfile

import (
	"os"
	"strings"
	"testing"
)

const los38qTelegramCapPath = "../../caps/bundled-caps/telegram/CAP.md"

func los38qTelegramBody(t *testing.T, script string) string {
	t.Helper()
	data, err := os.ReadFile(los38qTelegramCapPath)
	if err != nil {
		t.Fatalf("read bundled cap: %v", err)
	}
	cf, err := Parse(data)
	if err != nil {
		t.Fatalf("parse bundled cap: %v", err)
	}
	sc := cf.FindScript(script)
	if sc == nil {
		t.Fatalf("the bundled telegram cap has no %s script", script)
	}
	return sc.Body
}

// The daemon prepends set -e to a bash handler on the execute_cap path
// (handler_cap_exec.go, fireCapHandler). The reply handler answers its
// in-band commands (/sessions, /status, /use, /model, /models) with one curl
// and then exits 0. A bare curl whose send failed ended the branch first: the
// run died non-zero before its own log line, for a message nobody could have
// received. A failed send is a logged step, not an abort.
//
// The same rule covers an assignment: "SEND=$(curl ...)" takes the status of
// the substitution, so the check on the next line was dead code under set -e.
// That is why the test looks at every line that calls curl, not only at lines
// that start with it, and why the call itself has to carry the guard.
//
// The static shape is the contract here: the reply body is 300 lines and parses
// JSON through the yesmem CLI, so a full run would need a stub per call and
// would still not pin this property better than the line itself.
func TestLos38qTelegramReplyHasNoBareCurl(t *testing.T) {
	body := los38qTelegramBody(t, "telegram_reply")

	if !strings.Contains(body, "sendMessage") {
		t.Fatal("the reply body has no sendMessage call, the test would be vacuous")
	}
	for i, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "curl ") {
			continue
		}
		if !strings.Contains(trimmed, "||") {
			t.Errorf("telegram_reply:%d has a curl whose failure aborts the handler under set -e: %s", i+1, trimmed)
		}
	}
}

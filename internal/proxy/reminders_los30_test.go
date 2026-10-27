package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// los30PadTo appends filler turns after the given head messages and closes the
// thread with the current user turn. The resulting length sets the age of
// message 0: age = (len-1)/2 turns.
func los30PadTo(head []any, total int) []any {
	msgs := append([]any{}, head...)
	for len(msgs) < total-1 {
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "ok"})
		if len(msgs) < total-1 {
			msgs = append(msgs, map[string]any{"role": "user", "content": "filler"})
		}
	}
	return append(msgs, map[string]any{"role": "user", "content": "latest"})
}

const los30FileChangeDiff = "Note: /home/user/memory/yesmem/internal/proxy/proxy.go was modified, either by the user or by a linter.\nHere are the relevant changes (shown with line numbers):\n     1→package proxy\n     2→import (\n     3→    \"bytes\"\n"

// YM-139: the reminder age must be a property of this thread's own history.
// Before the fix the age was computed from the proxy-wide request counter, so
// the very same thread content was aged differently depending on how much
// traffic OTHER threads produced (and reset to zero on restart).
func TestStripReminders_AgeIsThreadLocal(t *testing.T) {
	msgs := los30PadTo([]any{
		map[string]any{"role": "user", "content": fmt.Sprintf("fix it <system-reminder>\n%s\n</system-reminder>", los30FileChangeDiff)},
	}, 19)

	// age of message 0 = (19-1)/2 = 9 turns → the 3..10 band, i.e. the summary
	// with line ranges. The tail form ("[file-changed: proxy.go]") would mean
	// the age had jumped past 10, the full diff would mean it stayed below 3.
	result := StripReminders(msgs)

	text, _ := result[0].(map[string]any)["content"].(string)

	if strings.Contains(text, "package proxy") {
		t.Errorf("a 9-turn-old file-change diff must be summarized, got full diff: %q", text)
	}
	if !strings.Contains(text, "[file-changed: proxy.go L1-3]") {
		t.Errorf("expected the summary band with line ranges, got: %q", text)
	}
}

// The tail message keeps its full diff: age 0 must never be treated as old.
func TestStripReminders_TailKeepsFullDiff(t *testing.T) {
	msgs := los30PadTo([]any{
		map[string]any{"role": "user", "content": "hello"},
	}, 21)
	msgs[len(msgs)-1] = map[string]any{
		"role":    "user",
		"content": fmt.Sprintf("do it <system-reminder>\n%s\n</system-reminder>", los30FileChangeDiff),
	}

	result := StripReminders(msgs)

	tailText, _ := result[len(result)-1].(map[string]any)["content"].(string)
	if !strings.Contains(tailText, "package proxy") {
		t.Errorf("the current turn's file-change diff must stay intact, got: %q", tailText)
	}
}

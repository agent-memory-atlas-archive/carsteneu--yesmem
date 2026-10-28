package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

type mockDaemonCall struct {
	method string
	params map[string]any
}

type mockDaemon struct {
	calls   []mockDaemonCall
	respond map[string]json.RawMessage
	errOn   map[string]error
}

func (m *mockDaemon) query(method string, params map[string]any) (json.RawMessage, error) {
	m.calls = append(m.calls, mockDaemonCall{method, params})
	if err, ok := m.errOn[method]; ok {
		return nil, err
	}
	if r, ok := m.respond[method]; ok {
		return r, nil
	}
	return nil, nil
}

func makeAssistantRepl(code string) map[string]any {
	return map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"name":  "REPL",
				"input": map[string]any{"code": code},
			},
		},
	}
}

func TestExtractShellCommandsFromMessages_Empty(t *testing.T) {
	cmds := extractShellCommandsFromMessages(nil)
	if len(cmds) != 0 {
		t.Errorf("expected 0 cmds, got %d", len(cmds))
	}
}

func TestExtractShellCommandsFromMessages_IgnoresUserRole(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  "REPL",
					"input": map[string]any{"code": "await sh('echo foo')"},
				},
			},
		},
	}
	cmds := extractShellCommandsFromMessages(messages)
	if len(cmds) != 0 {
		t.Errorf("expected 0 (user-role blocks ignored), got %d: %v", len(cmds), cmds)
	}
}

func TestExtractShellCommandsFromMessages_ExtractsThreeQuoteStyles(t *testing.T) {
	messages := []any{
		makeAssistantRepl("o.a = await sh('echo foo'); o.b = await sh(`echo bar`); o.c = await sh(\"echo baz\")"),
	}
	cmds := extractShellCommandsFromMessages(messages)
	if len(cmds) != 3 {
		t.Fatalf("expected 3 sh commands, got %d: %v", len(cmds), cmds)
	}
	want := []string{"echo foo", "echo bar", "echo baz"}
	for i, w := range want {
		if cmds[i] != w {
			t.Errorf("[%d] got %q, want %q", i, cmds[i], w)
		}
	}
}

func TestExtractShellCommandsFromMessages_IgnoresNonREPLTools(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  "Bash",
					"input": map[string]any{"command": "sh('foo')"},
				},
			},
		},
	}
	cmds := extractShellCommandsFromMessages(messages)
	if len(cmds) != 0 {
		t.Errorf("expected 0 (non-REPL tool ignored), got %d: %v", len(cmds), cmds)
	}
}

// YM-147: only the newest assistant turn is read. The older turns were already
// recorded by the requests that followed them; reading them again on every
// request is what fired one RPC per historical command.
func TestExtractShellCommandsFromMessages_MultiMessage(t *testing.T) {
	messages := []any{
		makeAssistantRepl("await sh('echo one')"),
		map[string]any{"role": "user", "content": "user reply"},
		makeAssistantRepl("await sh('echo two'); await sh('echo three')"),
	}
	cmds := extractShellCommandsFromMessages(messages)
	if len(cmds) != 2 {
		t.Fatalf("expected the 2 commands of the newest turn, got %d: %v", len(cmds), cmds)
	}
	if cmds[0] != "echo two" || cmds[1] != "echo three" {
		t.Errorf("expected the newest turn's commands, got %v", cmds)
	}
}

func TestDetectReplPatternSuggestion_NoCommandsReturnsNil(t *testing.T) {
	m := &mockDaemon{}
	sug := detectReplPatternSuggestion(nil, "proj", m.query)
	if sug != nil {
		t.Errorf("expected nil, got %+v", sug)
	}
	if len(m.calls) != 0 {
		t.Errorf("expected 0 daemon calls, got %d", len(m.calls))
	}
}

func TestDetectReplPatternSuggestion_EmptyProjectSkipsAll(t *testing.T) {
	m := &mockDaemon{}
	messages := []any{makeAssistantRepl("await sh('foo')")}
	sug := detectReplPatternSuggestion(messages, "", m.query)
	if sug != nil {
		t.Errorf("expected nil for empty project, got %+v", sug)
	}
	if len(m.calls) != 0 {
		t.Errorf("expected 0 daemon calls, got %d", len(m.calls))
	}
}

func TestDetectReplPatternSuggestion_RecordsButDoesNotReturnInlineSuggestion(t *testing.T) {
	respBody, _ := json.Marshal(map[string]any{
		"pattern": map[string]any{
			"id": 1, "project": "proj", "shape_hash": "abc123",
			"first_cmd_example": "echo foo", "count": 5, "dismiss_count": 0,
		},
	})
	m := &mockDaemon{
		respond: map[string]json.RawMessage{
			"get_repl_pattern_suggestion": respBody,
		},
	}
	messages := []any{makeAssistantRepl("await sh('sqlite3 db q')")}
	sug := detectReplPatternSuggestion(messages, "proj", m.query)
	if sug != nil {
		t.Fatalf("expected no inline suggestion, got %+v", sug)
	}
	if len(m.calls) != 1 {
		t.Fatalf("expected 1 daemon call (record only), got %d: %+v", len(m.calls), m.calls)
	}
	if m.calls[0].method != "record_repl_pattern" {
		t.Errorf("first call method: %s", m.calls[0].method)
	}
	if m.calls[0].params["project"] != "proj" {
		t.Errorf("record project param: %v", m.calls[0].params["project"])
	}
}

func TestDetectReplPatternSuggestion_NullResponseReturnsNil(t *testing.T) {
	m := &mockDaemon{
		respond: map[string]json.RawMessage{
			"get_repl_pattern_suggestion": json.RawMessage("null"),
		},
	}
	messages := []any{makeAssistantRepl("await sh('sqlite3 db q')")}
	sug := detectReplPatternSuggestion(messages, "proj", m.query)
	if sug != nil {
		t.Errorf("expected nil for null response, got %+v", sug)
	}
	if len(m.calls) != 1 || m.calls[0].method != "record_repl_pattern" {
		t.Fatalf("expected only record_repl_pattern call, got %+v", m.calls)
	}
}

func TestDetectReplPatternSuggestion_DaemonErrorReturnsNil(t *testing.T) {
	m := &mockDaemon{}
	messages := []any{makeAssistantRepl("await sh('sqlite3 db q')")}
	sug := detectReplPatternSuggestion(messages, "proj", m.query)
	if sug != nil {
		t.Errorf("expected nil for record-only path, got %+v", sug)
	}
	if len(m.calls) != 1 || m.calls[0].method != "record_repl_pattern" {
		t.Fatalf("expected only record_repl_pattern call, got %+v", m.calls)
	}
}

func TestHasRecentREPLActivity_DetectsInLastPosition(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "text"},
		makeAssistantRepl("await sh('echo foo')"),
	}
	if !hasRecentREPLActivity(messages, 20) {
		t.Error("expected true for REPL in last position")
	}
}

func TestHasRecentREPLActivity_FalseForPureText(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi there"},
		map[string]any{"role": "user", "content": "thanks"},
	}
	if hasRecentREPLActivity(messages, 20) {
		t.Error("expected false when no REPL tool_use present")
	}
}

func TestHasRecentREPLActivity_IgnoresOutsideWindow(t *testing.T) {
	// 25-msg list: REPL at position 0, window=20 → only positions 5..24 scanned.
	messages := make([]any, 25)
	messages[0] = makeAssistantRepl("await sh('ancient')")
	for i := 1; i < 25; i++ {
		messages[i] = map[string]any{"role": "user", "content": "filler"}
	}
	if hasRecentREPLActivity(messages, 20) {
		t.Error("expected false — REPL at idx 0 is outside window=20 over 25-msg list")
	}
}

func TestHasRecentREPLActivity_IgnoresUserRoleREPL(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  "REPL",
					"input": map[string]any{"code": "await sh('foo')"},
				},
			},
		},
	}
	if hasRecentREPLActivity(messages, 20) {
		t.Error("expected false — user-role REPL blocks should not gate-open")
	}
}

func TestDetectReplPatternSuggestion_NoREPLActivityShortCircuits(t *testing.T) {
	m := &mockDaemon{}
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi"},
	}
	sug := detectReplPatternSuggestion(messages, "proj", m.query)
	if sug != nil {
		t.Errorf("expected nil (no REPL activity), got %+v", sug)
	}
	if len(m.calls) != 0 {
		t.Errorf("expected 0 daemon calls (activity-gate short-circuit), got %d: %+v", len(m.calls), m.calls)
	}
}

func TestDetectReplPatternSuggestion_SkipsTrivialShapesEntirely(t *testing.T) {
	m := &mockDaemon{}
	messages := []any{
		makeAssistantRepl("await sh('echo hi'); await sh('ls -la'); await sh('pwd')"),
	}
	sug := detectReplPatternSuggestion(messages, "proj", m.query)
	if sug != nil {
		t.Errorf("expected nil (all shapes trivial), got %+v", sug)
	}
	for _, c := range m.calls {
		if c.method == "record_repl_pattern" {
			t.Errorf("trivial shape recorded, should have been filtered: %+v", c.params)
		}
		if c.method == "get_repl_pattern_suggestion" {
			t.Errorf("get_suggestion called despite 0 non-trivial records: %+v", c.params)
		}
	}
}

func TestExtractMatchedCap(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{"reddit_posts", `sqlite3 ~/.claude/yesmem/caps.db "SELECT * FROM cap_reddit__posts LIMIT 5"`, "reddit"},
		{"reddit_comments", "sqlite3 caps.db 'SELECT body FROM cap_reddit__comments WHERE post_permalink LIKE ?'", "reddit"},
		{"reddit_links", "sqlite3 caps.db .schema cap_reddit__links", "reddit"},
		{"telegram_updates", "sqlite3 caps.db SELECT FROM cap_telegram__updates", "telegram"},
		{"underscored_cap", "sqlite3 caps.db SELECT FROM cap_gluten_shop_tools__results", "gluten_shop_tools"},
		{"no_cap_table", "git status --porcelain", ""},
		{"plain_sqlite", `sqlite3 db ".tables"`, ""},
		{"non_cap_table", "sqlite3 yesmem.db SELECT FROM learnings", ""},
		{"empty", "", ""},
		{"first_match_wins", "SELECT FROM cap_a__t1 JOIN cap_b__t2", "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMatchedCap(tt.cmd)
			if got != tt.want {
				t.Errorf("extractMatchedCap(%q) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestDetectReplPatternSuggestion_MixedTrivialAndRealRecordsOnlyReal(t *testing.T) {
	respBody, _ := json.Marshal(map[string]any{
		"pattern": map[string]any{
			"id": 1, "project": "proj", "shape_hash": "abc",
			"first_cmd_example": "sqlite3 db q", "count": 5,
		},
	})
	m := &mockDaemon{
		respond: map[string]json.RawMessage{
			"get_repl_pattern_suggestion": respBody,
		},
	}
	messages := []any{
		makeAssistantRepl("await sh('echo hi'); await sh('sqlite3 db q'); await sh('ls -la')"),
	}
	sug := detectReplPatternSuggestion(messages, "proj", m.query)
	if sug != nil {
		t.Fatalf("expected no inline suggestion, got %+v", sug)
	}
	recordCalls := 0
	for _, c := range m.calls {
		if c.method == "record_repl_pattern" {
			recordCalls++
			ex, _ := c.params["example"].(string)
			if ex == "echo hi" || ex == "ls -la" {
				t.Errorf("trivial shape recorded: %q", ex)
			}
		}
	}
	if recordCalls != 1 {
		t.Errorf("expected exactly 1 record call (only 'sqlite3 db q' is non-trivial), got %d", recordCalls)
	}
}

func TestInjectReminderIntoLastUserMessage_StringContent(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi"},
		map[string]any{"role": "user", "content": "next prompt"},
	}}
	ok := injectReminderIntoLastUserMessage(req, "BODY")
	if !ok {
		t.Fatal("expected ok=true")
	}
	msgs := req["messages"].([]any)
	last := msgs[2].(map[string]any)
	got, _ := last["content"].(string)
	want := "next prompt\n\n<system-reminder>\nBODY\n</system-reminder>"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	first := msgs[0].(map[string]any)
	if first["content"] != "hello" {
		t.Error("earlier user message must not be modified")
	}
}

func TestInjectReminderIntoLastUserMessage_BlockListContent(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "user text"},
		}},
	}}
	ok := injectReminderIntoLastUserMessage(req, "B")
	if !ok {
		t.Fatal("expected ok=true")
	}
	msgs := req["messages"].([]any)
	blocks := msgs[0].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	tail := blocks[1].(map[string]any)
	if tail["type"] != "text" {
		t.Errorf("appended block type: %v", tail["type"])
	}
	txt, _ := tail["text"].(string)
	if !strings.Contains(txt, "<system-reminder>") || !strings.Contains(txt, "B") {
		t.Errorf("appended text missing reminder body: %q", txt)
	}
}

func TestInjectReminderIntoLastUserMessage_NoUserMessage(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": "only assistant"},
	}}
	ok := injectReminderIntoLastUserMessage(req, "B")
	if ok {
		t.Error("expected ok=false when no user message present")
	}
	msgs := req["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "only assistant" {
		t.Error("messages slice must not be mutated when no user message found")
	}
}

func TestFormatReplPatternSuggestion_MentionsCapAndDismiss(t *testing.T) {
	sug := ReplPatternSuggestion{
		ID:              42,
		Project:         "myproj",
		ShapeHash:       "deadbeefcafebabe",
		FirstCmdExample: "sqlite3 caps.db SELECT FROM cap_reddit__posts",
		Count:           7,
		MatchedCap:      "reddit",
	}
	body := formatReplPatternSuggestion(sug, "myproj")
	wants := []string{
		"7",
		"deadbeefcafebabe",
		"reddit",
		"cap_search",
		"dismiss_repl_pattern",
		"sqlite3 caps.db SELECT FROM cap_reddit__posts",
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("body missing %q\nbody=%s", w, body)
		}
	}
}

package proxy

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// codexUUIDForTest is a well-formed session uuid; the resolver only ever reads
// the last 36 characters of a rollout file name.
const codexUUIDForTest = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

const codexUUIDOther = "0b1d2f3a-4c5e-4f60-8a9b-0c1d2e3f4a5b"

// requireProc skips on platforms without /proc/<pid>/fd. The CI matrix runs
// macOS too, where the lookup cannot work; the skip is the convention this
// package already uses (session_pid_test.go).
func requireProc(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("no /proc/<pid>/fd on " + runtime.GOOS)
	}
}

// openRollout points the resolver at a temp directory and keeps one rollout
// file open, so /proc/<pid>/fd shows the link codex leaves open.
func openRollout(t *testing.T, uuid string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".codex", "sessions")
	prev := codexSessionsRoot
	codexSessionsRoot = func() string { return root }
	t.Cleanup(func() { codexSessionsRoot = prev })

	dir := filepath.Join(root, "2026", "10", "17")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "rollout-2026-10-17T10-59-57-"+uuid+".jsonl"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}

// A codex request carries no session header, so the parity path keyed it by a
// derived thread hash and its mail (addressed to codex:<uuid>) was never found
// (YM-235/AGT-MA32). The uuid is recoverable from the client process: codex
// holds its rollout jsonl open.
func TestCodexSessionUUIDForPID_ReadsTheRolloutLink(t *testing.T) {
	requireProc(t)
	openRollout(t, codexUUIDForTest)
	if got := codexSessionUUIDForPID(os.Getpid()); got != codexUUIDForTest {
		t.Fatalf("codexSessionUUIDForPID = %q, want %q", got, codexUUIDForTest)
	}
}

func TestCodexSessionUUIDFromLinks_OnlyACodexRolloutCounts(t *testing.T) {
	requireProc(t)
	openRollout(t, codexUUIDForTest)
	root := codexSessionsRoot()

	cases := map[string][]string{
		"rollout link": {"/dev/null", filepath.Join(root, "2026/10/17/rollout-2026-10-17T10-59-57-"+codexUUIDForTest+".jsonl")},
		// Any same-uid process can open a file with a rollout-shaped name. Only a
		// link below the directory codex writes may name a mailbox.
		"outside the codex root": {"/tmp/evil/.codex/sessions/2026/10/17/rollout-2026-10-17T10-59-57-" + codexUUIDForTest + ".jsonl"},
		"not a jsonl":            {filepath.Join(root, "2026/10/17/rollout-2026-10-17T10-59-57-"+codexUUIDForTest)},
		"truncated uuid":         {filepath.Join(root, "2026/10/17/rollout-2026-10-17T10-59-57-3fa85f64.jsonl")},
		"no rollout":             {"/dev/null", "socket:[123]"},
	}
	for name, links := range cases {
		want := ""
		if name == "rollout link" {
			want = codexUUIDForTest
		}
		if got := codexSessionUUIDFromLinks(links); got != want {
			t.Errorf("%s: codexSessionUUIDFromLinks = %q, want %q", name, got, want)
		}
	}
}

// Two open rollouts name two sessions. There is no tie-break that would be
// right, and delivering another session's mail is worse than delivering none.
func TestCodexSessionUUIDFromLinks_TwoRolloutsAreAmbiguous(t *testing.T) {
	requireProc(t)
	openRollout(t, codexUUIDForTest)
	root := codexSessionsRoot()
	links := []string{
		filepath.Join(root, "2026/10/17/rollout-2026-10-17T10-59-57-"+codexUUIDForTest+".jsonl"),
		filepath.Join(root, "2026/10/17/rollout-2026-10-17T11-00-01-"+codexUUIDOther+".jsonl"),
	}
	if got := codexSessionUUIDFromLinks(links); got != "" {
		t.Errorf("ambiguous rollouts resolved to %q, want \"\"", got)
	}
	// The same file linked twice is not ambiguous.
	dup := []string{links[0], links[0]}
	if got := codexSessionUUIDFromLinks(dup); got != codexUUIDForTest {
		t.Errorf("duplicate links resolved to %q, want %q", got, codexUUIDForTest)
	}
}

func TestCodexChannelAddress(t *testing.T) {
	requireProc(t)
	openRollout(t, codexUUIDForTest)
	if got := codexChannelAddress(os.Getpid()); got != "codex:"+codexUUIDForTest {
		t.Errorf("codexChannelAddress = %q, want codex:%s", got, codexUUIDForTest)
	}
	if got := codexChannelAddress(0); got != "" {
		t.Errorf("a request without a peer resolved to %q, want \"\"", got)
	}
	if got := codexChannelAddress(1); got != "" && os.Getpid() != 1 {
		t.Errorf("an unreadable process resolved to %q, want \"\"", got)
	}
}

// The injection names the identity the inbox was read under: that line is what
// the agent answers to. A codex session must see its own codex address there.
func TestAnnotateOpenAIMessageMetadata_ChannelUsesTheResolvedIdentity(t *testing.T) {
	s := &Server{
		timestampStore:     NewTimestampStore(),
		logger:             log.New(io.Discard, "", 0),
		channelInjectCount: map[string]int{},
	}
	var readAs string
	s.channelLoader = func(threadID, project string) dialogResult {
		readAs = threadID
		return dialogResult{Extra: "📨 Von orchestrator [command]:\nrun the tests", SessionID: threadID, HasUnread: true}
	}

	msgs := []any{map[string]any{"role": "user", "content": "hello"}}
	req := map[string]any{"messages": msgs}
	ctx := &openAIRequestContext{
		ThreadID:    "opencode:deadbeefdeadbeef",
		ChannelID:   "codex:" + codexUUIDForTest,
		Project:     "proj",
		ReqIdx:      1,
		RawMsgCount: 1,
	}

	s.annotateOpenAIMessageMetadata(req, ctx, msgs, "gpt-5.5", 1000)

	if readAs != "codex:"+codexUUIDForTest {
		t.Errorf("inbox read as %q, want the codex address", readAs)
	}
	got, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, "SESSION_ID: codex:"+codexUUIDForTest) {
		t.Errorf("the injection names the wrong reply target: %q", got)
	}
}

// A codex thread id is derived from message content (DeriveThreadID), so every
// codex session of one client version shares it. Freezing the mailbox into the
// per-message store under that key would replay one session's mail into the
// next session's prompt — including its SESSION_ID line.
func TestAnnotateOpenAIMessageMetadata_CodexMailIsNotFrozenUnderTheThreadID(t *testing.T) {
	const sharedThread = "opencode:0123456789abcdef"
	const mailA = "mail meant for session A only"

	s := &Server{
		cfg:                Config{FeatureDefaults: &config.FeatureGates{Timestamps: true}},
		timestampStore:     NewTimestampStore(),
		logger:             log.New(io.Discard, "", 0),
		channelInjectCount: map[string]int{},
	}
	s.channelLoader = func(threadID, project string) dialogResult {
		if threadID == "codex:"+codexUUIDForTest {
			return dialogResult{Extra: mailA, SessionID: threadID}
		}
		return dialogResult{}
	}

	// Session A: one user message, channel block resolved from its rollout.
	msgsA := []any{map[string]any{"role": "user", "content": "hello from A"}}
	ctxA := &openAIRequestContext{ThreadID: sharedThread, ChannelID: "codex:" + codexUUIDForTest, ReqIdx: 1, RawMsgCount: 1}
	s.annotateOpenAIMessageMetadata(map[string]any{"messages": msgsA}, ctxA, msgsA, "gpt-5.5", 1000)
	if got, _ := msgsA[0].(map[string]any)["content"].(string); !strings.Contains(got, mailA) {
		t.Fatalf("session A did not receive its own mail: %q", got)
	}

	// Session B: same derived thread id, different codex session, history of two
	// messages so the per-message store is replayed onto the earlier one.
	msgsB := []any{
		map[string]any{"role": "assistant", "content": "earlier reply"},
		map[string]any{"role": "user", "content": "hello from B"},
	}
	ctxB := &openAIRequestContext{ThreadID: sharedThread, ChannelID: "codex:" + codexUUIDOther, ReqIdx: 1, RawMsgCount: 2}
	s.annotateOpenAIMessageMetadata(map[string]any{"messages": msgsB}, ctxB, msgsB, "gpt-5.5", 1000)

	for i, m := range msgsB {
		if got, _ := m.(map[string]any)["content"].(string); strings.Contains(got, mailA) || strings.Contains(got, codexUUIDForTest) {
			t.Fatalf("message %d of session B carries session A's mailbox: %q", i, got)
		}
	}
}

// The handler line that sets the identity is the one the bug needed: without it
// the resolver works and nothing reaches the request. This drives the real
// handler and inspects what the upstream receives.
func TestOpenAIHandler_CodexRequestReadsItsOwnMailbox(t *testing.T) {
	requireProc(t)
	openRollout(t, codexUUIDForTest)

	var readAs, forwarded string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		forwarded = string(raw)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-5.5",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		})
	}))
	defer upstream.Close()

	s := fakeDaemon(t, func(method string, params map[string]any) string { return "null" })
	s.cfg.TargetURL = upstream.URL
	s.cfg.OpenAITargetURL = upstream.URL
	s.httpClient = http.DefaultClient
	s.timestampStore = NewTimestampStore()
	s.channelLoader = func(threadID, project string) dialogResult {
		readAs = threadID
		return dialogResult{Extra: "run the tests", SessionID: threadID}
	}

	prevPeer := peerSessionPIDFn
	peerSessionPIDFn = func(string, string) int { return os.Getpid() }
	t.Cleanup(func() { peerSessionPIDFn = prevPeer })

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-tui/0.153.4 (linux; x86_64)")

	s.handleOpenAICompletions(httptest.NewRecorder(), req)

	want := "codex:" + codexUUIDForTest
	if readAs != want {
		t.Fatalf("handler read the inbox as %q, want %q", readAs, want)
	}
	if !strings.Contains(forwarded, "SESSION_ID: "+want) {
		t.Errorf("the forwarded request does not name the codex address: %s", forwarded)
	}
	if !strings.Contains(forwarded, "run the tests") {
		t.Errorf("the forwarded request carries no mail: %s", forwarded)
	}
}

// Codex speaks the Responses API, so the same wiring lives in the second
// handler. Without its own test, deleting that line leaves the suite green.
func TestResponsesHandler_CodexRequestReadsItsOwnMailbox(t *testing.T) {
	requireProc(t)
	openRollout(t, codexUUIDForTest)

	var readAs, forwarded string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		forwarded = string(raw)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "object": "response", "model": "gpt-5.5", "status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
		})
	}))
	defer upstream.Close()

	s := fakeDaemon(t, func(method string, params map[string]any) string { return "null" })
	s.cfg.TargetURL = upstream.URL
	s.cfg.OpenAITargetURL = upstream.URL
	s.httpClient = http.DefaultClient
	s.timestampStore = NewTimestampStore()
	s.channelLoader = func(threadID, project string) dialogResult {
		readAs = threadID
		return dialogResult{Extra: "run the tests", SessionID: threadID}
	}

	prevPeer := peerSessionPIDFn
	peerSessionPIDFn = func(string, string) int { return os.Getpid() }
	t.Cleanup(func() { peerSessionPIDFn = prevPeer })

	body := `{"model":"gpt-5.5","input":"hello","stream":false}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-tui/0.153.4 (linux; x86_64)")

	s.handleResponses(httptest.NewRecorder(), req)

	want := "codex:" + codexUUIDForTest
	if readAs != want {
		t.Fatalf("handler read the inbox as %q, want %q", readAs, want)
	}
	if !strings.Contains(forwarded, "SESSION_ID: "+want) || !strings.Contains(forwarded, "run the tests") {
		t.Errorf("the forwarded request does not carry the codex mail: %s", forwarded)
	}
}

// Freezing is only safe when the thread id names a session. The header-carrying
// harnesses must keep it (that is the YM-59 cache property); a codex thread id
// is shared by every codex session and must never carry per-session content,
// including when the identity could not be resolved.
func TestAnnotateOpenAIMessageMetadata_FreezesOnlyWhenTheThreadIsASession(t *testing.T) {
	cases := []struct {
		name      string
		isSession bool
	}{
		{"header harness (thread id is a session)", true},
		{"derived thread id (codex, unresolved)", false},
	}
	for _, c := range cases {
		s := &Server{
			cfg:                Config{FeatureDefaults: &config.FeatureGates{Timestamps: true}},
			timestampStore:     NewTimestampStore(),
			logger:             log.New(io.Discard, "", 0),
			channelInjectCount: map[string]int{},
		}
		s.channelLoader = func(threadID, project string) dialogResult {
			return dialogResult{Extra: "mail", SessionID: threadID}
		}
		msgs := []any{map[string]any{"role": "user", "content": "hello"}}
		ctx := &openAIRequestContext{ThreadID: "opencode:abcdefabcdefabcd", ThreadIsSession: c.isSession, ReqIdx: 1, RawMsgCount: 1}

		s.annotateOpenAIMessageMetadata(map[string]any{"messages": msgs}, ctx, msgs, "gpt-5.5", 1000)

		meta, ok := s.timestampStore.Get(ctx.ThreadID, 1)
		frozen := ok && meta.Channel != ""
		if frozen != c.isSession {
			t.Errorf("%s: frozen=%v, want %v", c.name, frozen, c.isSession)
		}
		if got, _ := msgs[0].(map[string]any)["content"].(string); !strings.Contains(got, "mail") {
			t.Errorf("%s: the request itself did not receive the mail: %q", c.name, got)
		}
	}
}

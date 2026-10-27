package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// YM-290 (BRF-I2, Pin 4): the identity line inside the channel block was the
// German "DEINE_SESSION_ID" while every other injected string in this pipeline
// is language-neutral. The line names the reply target an agent must use for
// send_to, so it stays a plain "SESSION_ID".
const los27bSessionIdLine = "SESSION_ID: "

func TestLos27b_OpenAIChannelNamesSessionNeutral(t *testing.T) {
	s := &Server{
		timestampStore:     NewTimestampStore(),
		logger:             log.New(io.Discard, "", 0),
		channelInjectCount: map[string]int{},
	}
	s.channelLoader = func(threadID, project string) dialogResult {
		return dialogResult{Extra: "📨 Von orchestrator [command]:\nrun the tests", SessionID: threadID}
	}

	msgs := []any{map[string]any{"role": "user", "content": "hello"}}
	req := map[string]any{"messages": msgs}
	ctx := &openAIRequestContext{ThreadID: "ses_los27b_neutral", Project: "proj", ReqIdx: 1, RawMsgCount: 1}

	s.annotateOpenAIMessageMetadata(req, ctx, msgs, "glm-5.2", 1000)

	got, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, los27bSessionIdLine+"ses_los27b_neutral") {
		t.Errorf("the injected block does not carry the neutral identity line: %q", got)
	}
	if strings.Contains(got, "DEINE_SESSION_ID") {
		t.Errorf("the injected block still carries the German token: %q", got)
	}
}

func TestLos27b_AnthropicChannelNamesSessionNeutral(t *testing.T) {
	var upstream string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstream = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	defer backend.Close()

	s := los26aHandlerServer(t, backend.URL)
	s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true}
	// The Anthropic path reads its channel content through checkDialogMessages,
	// which has no test seam of its own: stub the daemon instead.
	fd := fakeDaemon(t, func(method string, params map[string]any) string {
		if method == "check_channel" {
			return `{"messages":[{"sender":"orchestrator","content":"run the tests","msg_type":"command"}]}`
		}
		return "null"
	})
	s.cfg.DataDir = fd.cfg.DataDir
	s.daemonReady.Store(true)

	const sessionID = "ses_los27b_anthropic"
	body := `{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},` +
		`"system":[{"type":"text","text":"Primary working directory: /home/tester/memory/yesmem"}],` +
		`"messages":[{"role":"user","content":"start the task"}]}`
	reqx := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	reqx.Header.Set("X-Claude-Code-Session-Id", sessionID)
	rec := httptest.NewRecorder()

	s.handleMessages(rec, reqx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(upstream, "run the tests") {
		t.Fatalf("the dialog content never reached the upstream request: %s", upstream)
	}
	if !strings.Contains(upstream, los27bSessionIdLine+sessionID) {
		t.Errorf("the dialog block does not carry the neutral identity line: %s", upstream)
	}
	if strings.Contains(upstream, "DEINE_SESSION_ID") {
		t.Errorf("the dialog block still carries the German token: %s", upstream)
	}
}

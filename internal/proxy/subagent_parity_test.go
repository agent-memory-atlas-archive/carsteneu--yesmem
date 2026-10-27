package proxy

import (
	"encoding/json"
	"net/http"
	"testing"
)

// subagentShape is one client declaration, fed unchanged to both call sites.
type subagentShape struct {
	name     string
	header   http.Header
	messages []any
	req      map[string]any
	want     bool
}

func los461Shapes() []subagentShape {
	msgs := []any{map[string]any{"role": "user", "content": "hello"}}
	cliSystem := []any{map[string]any{"type": "text", "text": "cc_entrypoint=cli"}}
	parent := func() http.Header {
		h := http.Header{}
		h.Set("X-Opencode-Parent-Session-Id", "ses_parent")
		return h
	}
	agentTypeHeader := func() http.Header {
		h := http.Header{}
		h.Set("X-Opencode-Agent-Type", "subagent")
		return h
	}
	ocSession := func() http.Header {
		h := http.Header{}
		h.Set("x-opencode-session-id", "ses_main")
		return h
	}

	return []subagentShape{
		{
			name:     "parent-session-header",
			header:   parent(),
			messages: msgs,
			req:      map[string]any{"model": "gpt-5.4", "thinking": map[string]any{"type": "adaptive"}},
			want:     true,
		},
		{
			// A legacy fork build labels the child with the agent-type header and
			// sends no parent session. The OpenAI path never looked at this header,
			// so the same child was a subagent on /v1/messages and a main session on
			// /v1/chat/completions.
			name:     "agent-type-header-without-parent",
			header:   agentTypeHeader(),
			messages: msgs,
			req:      map[string]any{"model": "gpt-5.4"},
			want:     true,
		},
		{
			name:     "sdk-ts-entrypoint",
			header:   http.Header{},
			messages: msgs,
			req:      map[string]any{"model": "gpt-5.4", "system": []any{map[string]any{"text": "cc_entrypoint=sdk-ts"}}},
			want:     true,
		},
		{
			// The model marker outranks the main-session marker on both wires.
			name:     "haiku-model-with-cli-marker",
			header:   http.Header{},
			messages: msgs,
			req:      map[string]any{"model": "claude-haiku-4-5-20251001", "system": cliSystem},
			want:     true,
		},
		{
			name:     "declared-agent-type",
			header:   http.Header{},
			messages: msgs,
			req:      map[string]any{"model": "gpt-5.4", "system": cliSystem, "metadata": map[string]any{"user_id": `{"agent_type":"subagent"}`}},
			want:     true,
		},
		{
			name:     "cli-main-session-without-thinking",
			header:   http.Header{},
			messages: msgs,
			req:      map[string]any{"model": "gpt-5.4", "system": cliSystem},
			want:     false,
		},
		{
			name:     "opencode-main-session-without-thinking",
			header:   ocSession(),
			messages: msgs,
			req:      map[string]any{"model": "glm-5.2"},
			want:     false,
		},
	}
}

// YM-461 (PRX-P19b): both wires call one predicate, so a client that declares
// itself gets the same verdict on /v1/messages and on the OpenAI parity path.
// The expressions mirror the two call sites (proxy.go:1642 and
// openai_parity.go:prepareOpenAIRequestContext), so a revert of either the
// header set or the precedence cannot pass silently.
func TestYM461_DeclaredClientsAgreeAcrossWires(t *testing.T) {
	for _, shape := range los461Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			anthropic := isSubagentRequest(shape.header, shape.messages, shape.req)
			if anthropic != shape.want {
				t.Fatalf("anthropic wire = %v, want %v", anthropic, shape.want)
			}
			if openai := isOpenAIProxySubagent(shape.header, shape.messages, shape.req); openai != shape.want {
				t.Fatalf("openai wire = %v, want %v (same declaration, other endpoint)", openai, shape.want)
			}
		})
	}
}

// The parity path must also reach the shared verdict through its real entry
// point, not only through the wrapper: prepareOpenAIRequestContext carries the
// request headers now, so the agent-type header and the main-session markers
// arrive there too.
func TestYM461_ParityContextUsesTheSharedVerdict(t *testing.T) {
	s := los26aSawtoothServer(200000, nil)
	msgs := []any{map[string]any{"role": "user", "content": "hello"}}
	child := map[string]any{"model": "gpt-5.4", "messages": msgs}

	h := http.Header{}
	h.Set("X-Opencode-Agent-Type", "subagent")
	if ctx := s.prepareOpenAIRequestContext(child, 1, "", "", "opencode/1.2.3", h); !ctx.Subagent {
		t.Error("prepareOpenAIRequestContext ignored the agent-type header that the Anthropic path honours")
	}

	main := http.Header{}
	main.Set("x-opencode-session-id", "ses_main")
	if ctx := s.prepareOpenAIRequestContext(child, 2, "", "ses_main", "opencode/1.2.3", main); ctx.Subagent {
		t.Error("prepareOpenAIRequestContext classified an opencode main session as a subagent")
	}

	// Codex declares nothing at all: no session header, no cc_entrypoint. It must
	// stay a main session, otherwise briefing, stubs, collapse, the emergency
	// brake and the fork budget are off for every codex run.
	if ctx := s.prepareOpenAIRequestContext(child, 3, "", "", "codex-tui/0.132.0 (linux mint)", nil); ctx.Subagent {
		t.Error("a markerless codex request was classified as a subagent")
	}
}

// The markerless shape is where the two wires legitimately differ, and this is
// the pin that keeps the difference honest:
//
//   - The Anthropic request shape carries a thinking field, and a client that
//     declares nothing else while omitting it is a Claude Code subagent (YM-263
//     kept that fallback on purpose, los26b K1).
//   - The OpenAI shapes carry no thinking field at all: openai_translate writes
//     reasoning_content as a content block inside a message, never as req
//     ["thinking"]. Applied verbatim, the fallback would classify every
//     markerless /v1/chat/completions and /v1/responses request as a subagent,
//     and Codex sends neither a session header nor cc_entrypoint. That is the
//     "main sessions must never be classified as subagents" case.
//
// So the fallback is applied identically wherever the wire can carry it: on
// every request that has the field, and never on a wire that has no such field.
func TestYM461_MarkerlessClientKeepsTheWireSemantics(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hello"}}
	markerless := map[string]any{"model": "gpt-5.4", "messages": msgs}

	if isOpenAIProxySubagent(http.Header{}, msgs, markerless) {
		t.Error("markerless OpenAI request was classified as a subagent: briefing, stubs, collapse and the emergency brake would be off for a real session")
	}

	// Counter-check: the same client on the Anthropic shape keeps the fallback.
	anthropicMarkerless := map[string]any{"model": "claude-opus-4-6"}
	if !isSubagentRequest(http.Header{}, msgs, anthropicMarkerless) {
		t.Error("the Anthropic thinking fallback is part of the shared verdict and must stay in force")
	}

	// A request that asks for thinking is a main session on both wires. On the
	// OpenAI shape the field cannot occur, so the second assertion pins the
	// agreement of the verdict, not the fallback: there the early return for a
	// wire without a thinking field decides, and it decides "main session".
	withThinking := map[string]any{"model": "gpt-5.4", "thinking": map[string]any{"type": "adaptive"}}
	if isOpenAIProxySubagent(http.Header{}, msgs, withThinking) {
		t.Error("a request that asks for thinking is a main session on both wires")
	}
	if isSubagentRequest(http.Header{}, msgs, withThinking) {
		t.Error("a request that asks for thinking is a main session on both wires")
	}
}

// The wire flag rests on a property of the translator: the OpenAI shapes never
// produce a top-level "thinking" key (reasoning_content becomes a content block
// inside a message). If that ever changes, the fallback would stay silent where
// it could speak, which is the failure this test makes loud.
func TestYM461_OpenAITranslationNeverSetsAThinkingField(t *testing.T) {
	body := `{"model":"deepseek-reasoner","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"ok","reasoning_content":"thought"}]}`
	var oaiReq OpenAIChatRequest
	if err := json.Unmarshal([]byte(body), &oaiReq); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	anthReq, err := translateOpenAIToAnthropic(oaiReq)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if _, ok := anthReq["thinking"]; ok {
		t.Fatal("the OpenAI translation set a top-level thinking field; subagentVerdict's wire flag is now wrong")
	}
}

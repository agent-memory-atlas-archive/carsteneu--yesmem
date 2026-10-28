package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// los36HandlerLoopBody is one identical Edit cycle in the Anthropic wire
// format, repeated so DetectLoop fires (cycle length 2, two repetitions).
func los36HandlerLoopBody() string {
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-4-6","stream":false,"thinking":{"type":"adaptive"},"messages":[`)
	b.WriteString(`{"role":"user","content":"fix it"}`)
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&b, `,{"role":"assistant","content":[{"type":"tool_use","id":"t%d","name":"Edit","input":{"file_path":"/src/a.go","old_string":"a","new_string":"b"}}]}`, i)
		fmt.Fprintf(&b, `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":"edited"}]}`, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

func los36WarningCount(s *Server) int {
	total := 0
	for _, state := range s.loopStates {
		total += state.WarningCount
	}
	return total
}

// YM-132: the Anthropic path must skip loop detection for subagents too. The
// OpenAI path is covered by TestOpenAIPipeline_LoopDetection_SkipsSubagent; this
// is the same guard on handleMessages, where the decision is the parent-session
// header.
func TestHandleMessages_LoopStateUntouchedForSubagent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parent     string
		wantWarned bool
	}{
		{name: "subagent-skipped", parent: "ses_parent", wantWarned: false},
		{name: "main-session-warns", parent: "", wantWarned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":1}}`))
			}))
			defer backend.Close()

			s := los26aHandlerServer(t, backend.URL)
			s.cfg.FeatureDefaults = &config.FeatureGates{Timestamps: true, LoopWarning: true}

			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(los36HandlerLoopBody()))
			req.Header.Set("X-Claude-Code-Session-Id", "ses_los36_dialect")
			if tc.parent != "" {
				req.Header.Set("X-Opencode-Parent-Session-Id", tc.parent)
			}
			rec := httptest.NewRecorder()

			s.handleMessages(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			got := los36WarningCount(s)
			if tc.wantWarned && got == 0 {
				t.Error("a main-session loop must raise the warning count")
			}
			if !tc.wantWarned && got != 0 {
				t.Errorf("a subagent request must not raise the loop warning count, got %d", got)
			}
		})
	}
}

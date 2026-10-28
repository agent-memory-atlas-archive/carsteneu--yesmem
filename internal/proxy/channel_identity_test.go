package proxy

import (
	"net/http"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

// The Anthropic path is the only live channel-injection path (proxy.go:1647,
// checkDialogMessages(threadID, …)). It must resolve one exact address per
// harness, because that address is what the read side matches on:
// Claude Code sends X-Claude-Code-Session-Id (bare uuid), opencode sends one of
// the opencode session headers and gets the "opencode:" namespace.
//
// The OpenAI-parity path does not call checkDialogMessages at all (the block is
// disabled at openai_parity.go:341-406) — see Phase 4/6 notes.
func TestAnthropicThreadID_ExactAddressPerHarness(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		req     map[string]any
		want    string
	}{
		{
			name:    "claude: X-Claude-Code-Session-Id",
			headers: map[string]string{"X-Claude-Code-Session-Id": "a542b034-d69d-4e10-9553-a549eefa4d77"},
			want:    "a542b034-d69d-4e10-9553-a549eefa4d77",
		},
		{
			name:    "opencode: x-opencode-session",
			headers: map[string]string{"x-opencode-session": "ses_ebfd8cca4ffe51n2AGcKG04Gqw"},
			want:    "opencode:ses_ebfd8cca4ffe51n2AGcKG04Gqw",
		},
		{
			name:    "opencode: x-opencode-session-id",
			headers: map[string]string{"x-opencode-session-id": "ses_ebfd8cca4ffe51n2AGcKG04Gqw"},
			want:    "opencode:ses_ebfd8cca4ffe51n2AGcKG04Gqw",
		},
		{
			name:    "opencode: x-session-affinity fallback",
			headers: map[string]string{"x-session-affinity": "ses_ebfd8cca4ffe51n2AGcKG04Gqw"},
			want:    "opencode:ses_ebfd8cca4ffe51n2AGcKG04Gqw",
		},
		{
			name: "claude: metadata.user_id fallback",
			req: map[string]any{
				"metadata": map[string]any{"user_id": `{"session_id":"b1234567-aaaa-bbbb-cccc-dddddddddddd"}`},
			},
			want: "b1234567-aaaa-bbbb-cccc-dddddddddddd",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range c.headers {
				h.Set(k, v)
			}
			got, _ := anthropicThread(c.req, h)
			if got != c.want {
				t.Fatalf("anthropicThread = %q, want %q", got, c.want)
			}
			// The thread id must already be the canonical storage form, otherwise
			// the read side would look for a different string than send_to stored.
			if canonical := storage.CanonicalAddress(got); canonical != c.want {
				t.Errorf("CanonicalAddress(%q) = %q, want the unchanged address %q", got, canonical, c.want)
			}
		})
	}
}

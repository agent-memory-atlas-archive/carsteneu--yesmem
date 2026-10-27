package extraction

import (
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// TestIsDialogInjection_CurrentChannelFormat pins YM-74 (part b): the channel
// transport moved to a <channel source="yesmem-dialog"> wrapper whose messages
// read "📨 Von <sender> [<type>]:", and neither marker was recognised. Dialog and
// broadcast traffic therefore went into the extraction as if the user had written
// it, diluting the learnings with agent chatter.
func TestIsDialogInjection_CurrentChannelFormat(t *testing.T) {
	cases := map[string]string{
		"anthropic and openai path wrapper": "<channel source=\"yesmem-dialog\">\nSESSION_ID: ses_x\n📨 Von agent-1 [command]:\nhello\n</channel>",
		"opencode api push":                 "📨 Von agent-20261017-08 [status]:\nDONE: section complete",
		"broadcast channel line":            "📨 Von opencode [decision]:\nsandbox profile decided",
	}
	for name, content := range cases {
		if !isDialogInjection(content) {
			t.Errorf("%s: channel injection not recognised as dialog traffic", name)
		}
	}
}

// TestIsDialogInjection_KeepsUserText guards the other direction: ordinary user
// text must keep flowing into the extraction.
func TestIsDialogInjection_KeepsUserText(t *testing.T) {
	for _, content := range []string{
		"Bitte implementiere die Kaskade und teste sie.",
		"Der Emoji-Report zeigt 3 neue Nachrichten.", // mentions nothing channel-specific
		"Kannst du das nochmal erklaeren?",
	} {
		if isDialogInjection(content) {
			t.Errorf("user text %q was mistaken for a dialog injection", content)
		}
	}
}

// TestPreFilterMessages_DropsCurrentChannelInjection is the end-to-end side: the
// injected turn and the assistant reply to it must both leave the transcript.
func TestPreFilterMessages_DropsCurrentChannelInjection(t *testing.T) {
	msgs := []models.Message{
		{Role: "user", MessageType: "text", Content: "Implementiere X", Timestamp: time.Now()},
		{Role: "assistant", MessageType: "text", Content: "Mache ich.", Timestamp: time.Now()},
		{Role: "user", MessageType: "text", Content: "<channel source=\"yesmem-dialog\">\n📨 Von agent-1 [command]:\nstatus?\n</channel>", Timestamp: time.Now()},
		{Role: "assistant", MessageType: "text", Content: "Alles laeuft.", Timestamp: time.Now()},
		{Role: "user", MessageType: "text", Content: "Danke, weiter.", Timestamp: time.Now()},
	}

	got := PreFilterMessages(msgs)
	var kept []string
	for _, m := range got {
		kept = append(kept, m.Content)
	}
	joined := strings.Join(kept, "|")
	if strings.Contains(joined, "📨") || strings.Contains(joined, "Alles laeuft") {
		t.Errorf("channel injection and its reply survived the pre-filter: %q", joined)
	}
	if !strings.Contains(joined, "Implementiere X") || !strings.Contains(joined, "Danke, weiter.") {
		t.Errorf("user turns were dropped: %q", joined)
	}
}

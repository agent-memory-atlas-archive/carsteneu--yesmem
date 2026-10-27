package opencode

import (
	"context"
	"os"
	"testing"
	"time"
)

// liveDeliveryEnv opts into the write-path acceptance run against the
// developer's RUNNING opencode services. Off by default, like liveE2EEnv, and
// separate from it because this one is not read-only: it creates a session and
// files a turn into it.
//
// The session is created for the run and cannot be removed — the v2 API the
// spec describes has no session.delete — so the run logs its id.
const liveDeliveryEnv = "YESMEM_LIVE_OPENCODE_DELIVERY"

// TestLiveOpencodeV2_PromptUserTurnIsAccepted is the acceptance proof that the
// live service accepts the body the daemon sends. Everywhere else the request
// shape is a stub's assumption, and the spec forbids unknown fields
// (additionalProperties: false) — so a field this client invented would be
// rejected in production and nowhere else.
//
// It runs with resume=false and a freshly created session, so it wakes nothing
// and spends no model turn: the turn stays queued in the inbox, which is where
// the test reads it back.
func TestLiveOpencodeV2_PromptUserTurnIsAccepted(t *testing.T) {
	if os.Getenv(liveDeliveryEnv) == "" {
		t.Skipf("set %s=1 to run against the live opencode services", liveDeliveryEnv)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := FirstLiveService(home)
	if err != nil {
		t.Fatalf("no live opencode v2 service: %v", err)
	}
	t.Logf("live service %s (%s)", svc.URL, svc.Version)
	c := NewClient(svc)
	ctx := context.Background()

	// A session of this run's own: it belongs to nobody's conversation.
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/api/session", map[string]any{"title": "[yesmem live test] send_to delivery"}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}
	if created.Data.ID == "" {
		t.Fatal("session.create returned no id")
	}
	t.Logf("created throwaway session %s — the v2 API has no session.delete, so it stays", created.Data.ID)

	const text = "smoke test: session.prompt acceptance"
	id, err := c.PromptUserTurn(ctx, created.Data.ID, text, PromptOptions{
		Delivery: DeliveryQueue,
		Resume:   false,
		Metadata: map[string]any{"sender": "live-test", "msg_type": "command"},
	})
	if err != nil {
		t.Fatalf("PromptUserTurn: %v", err)
	}
	if id == "" {
		t.Fatal("the service accepted the turn but reported no inbox id")
	}
	t.Logf("filed inbox entry %s", id)

	// Read it back from the session's inbox, so the proof is the service's own
	// record rather than the write's echo. A queued turn with resume=false does
	// not reach the conversation until the session processes it — reading it
	// here is what "queue" promises.
	var inbox struct {
		Data []struct {
			ID       string `json:"id"`
			Delivery string `json:"delivery"`
			Payload  struct {
				Text     string         `json:"text"`
				Metadata map[string]any `json:"metadata"`
			} `json:"payload"`
		} `json:"data"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		inbox.Data = nil
		readErr := c.get(ctx, "/api/session/"+created.Data.ID+"/inbox", nil, &inbox)
		if readErr == nil {
			for _, entry := range inbox.Data {
				if entry.ID != id {
					continue
				}
				if entry.Delivery != DeliveryQueue {
					t.Errorf("inbox entry delivery = %q, want %q", entry.Delivery, DeliveryQueue)
				}
				if entry.Payload.Text != text {
					t.Errorf("inbox entry text = %q, want %q", entry.Payload.Text, text)
				}
				if entry.Payload.Metadata["sender"] != "live-test" {
					t.Errorf("metadata did not survive the round trip: %v", entry.Payload.Metadata)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("inbox entry %s not found (last read error: %v)", id, readErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

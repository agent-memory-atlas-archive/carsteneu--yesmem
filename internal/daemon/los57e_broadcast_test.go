package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// los57eBroadcastSender seeds the agent row the broadcast path needs to label a
// sender: the label comes from the kernel-reported peer, so the row has to carry
// the test process's pid.
func los57eBroadcastSender(t *testing.T, s *storage.Store, project string) int {
	t.Helper()
	peer := os.Getpid()
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-los57e-bc", Project: project, Section: "bc",
		SessionID: "los57e-sender", Status: "running", PID: peer,
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	return peer
}

// A name that resolves to nothing leaves a broadcast row no project ever reads,
// while the sender is told "broadcast_sent". The refusal has to name the value the
// caller sent, or the case cannot be found in operation.
func TestLos57eBroadcastRefusesAnUnresolvedProjectName(t *testing.T) {
	h, s := mustHandler(t)
	peer := los57eBroadcastSender(t, s, "ghost-proj")

	// Twice: the second call is served from the resolution cache, and the marker
	// has to survive that path too (cold review F1).
	for i := 0; i < 2; i++ {
		resp := h.Handle(Request{Method: "broadcast", Params: map[string]any{
			"project": "ghost-proj", "content": "hello", "_peer_pid": peer,
		}})
		if resp.Error == "" {
			t.Fatalf("call %d: a broadcast to a project that resolves to nothing must be refused", i+1)
		}
		if !strings.Contains(resp.Error, "ghost-proj") {
			t.Errorf("call %d: error %q does not name the short name the caller sent", i+1, resp.Error)
		}
	}
	if msgs, err := s.GetUnreadBroadcasts("los57e-reader", "ghost-proj"); err != nil {
		t.Fatalf("read broadcasts: %v", err)
	} else if len(msgs) != 0 {
		t.Fatalf("%d broadcast row(s) were stored under the unresolved name", len(msgs))
	}

	// A cwd that does not carry the name resolves to nothing as well, cached under
	// its own key.
	for i := 0; i < 2; i++ {
		resp := h.Handle(Request{Method: "broadcast", Params: map[string]any{
			"project": "ghost-elsewhere", "content": "hello", "_peer_pid": peer,
			"_cwd": "/srv/other-dir",
		}})
		if resp.Error == "" {
			t.Fatalf("call %d with a foreign cwd: the broadcast must be refused", i+1)
		}
		if !strings.Contains(resp.Error, "ghost-elsewhere") {
			t.Errorf("call %d with a foreign cwd: error %q does not name the short name", i+1, resp.Error)
		}
	}

	// The reader refuses the same way: an empty inbox for a project that does not
	// exist under that key reads like "no messages".
	check := h.Handle(Request{Method: "check_broadcasts", Params: map[string]any{
		"project": "ghost-proj", "session_id": "los57e-sender", "_peer_pid": peer,
	}})
	if check.Error == "" {
		t.Fatal("check_broadcasts must refuse an unresolved project name")
	}
}

// The same call works for a project whose short name resolves: sender and reader
// meet on the resolved path, which is what made the basename case silent.
func TestLos57eBroadcastUsesTheResolvedPathForShortNames(t *testing.T) {
	h, s := mustHandler(t)
	if err := s.UpsertSession(&models.Session{
		ID: "ses-los57e-proj", Project: "/srv/proj/test-proj", ProjectShort: "test-proj",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	peer := los57eBroadcastSender(t, s, "test-proj")

	resp := h.Handle(Request{Method: "broadcast", Params: map[string]any{
		"project": "test-proj", "content": "hello", "_peer_pid": peer,
	}})
	if resp.Error != "" {
		t.Fatalf("broadcast: %s", resp.Error)
	}

	msgs, err := s.GetUnreadBroadcasts("los57e-reader", "/srv/proj/test-proj")
	if err != nil {
		t.Fatalf("read broadcasts: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stored broadcasts under the resolved path = %d, want 1", len(msgs))
	}
}

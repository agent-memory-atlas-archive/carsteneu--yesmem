package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// YM-295 (3): the ticket cites handleBroadcast as able to store a bare basename.
// The dispatcher resolves the project before any handler runs (handler.go:533),
// so a short name that matches one tracked project is stored as that project's
// path and the broadcast reaches only that checkout. This test locks the state
// the finding was closed with, and would fail if a later dispatch change routed
// the raw name into the write again.
func TestLos32gBroadcastStoresTheResolvedProjectPath(t *testing.T) {
	h, s := mustHandler(t)
	isolateHome(t)
	if err := s.UpsertSession(&models.Session{
		ID: "sess-los32g-bc", Project: "/srv/los32g/bcproj", StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	h.activeSessionMu.Lock()
	h.activeSessionID = "bc-sender"
	h.activeSessionMu.Unlock()

	if err := s.AgentCreate(storage.Agent{
		ID: "agent-20261020-81", Project: "/srv/los32g/bcproj", Section: "bc",
		SessionID: "bc-sender", Status: "running", PID: os.Getpid(),
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}

	resp := h.Handle(Request{Method: "broadcast", Params: map[string]any{
		"project": "bcproj", "content": "hello",
		// The label is resolved from the kernel-reported peer (YM-271); the row
		// below is what that peer maps to.
		"_peer_pid": os.Getpid(),
	}})
	if resp.Error != "" {
		t.Fatalf("broadcast: %s", resp.Error)
	}

	msgs, err := s.GetUnreadBroadcasts("reader", "/srv/los32g/bcproj")
	if err != nil {
		t.Fatalf("read broadcasts: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the resolved project must receive the broadcast, got %d", len(msgs))
	}
	if got, err := s.GetUnreadBroadcasts("reader-2", "bcproj"); err == nil && len(got) != 0 {
		t.Errorf("the bare name must not be an address of its own: %v", got)
	}
}

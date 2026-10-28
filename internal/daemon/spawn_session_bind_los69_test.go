package daemon

// The bind of a new opencode agent must not depend on prompt text. A fresh spawn
// used to be identified only by a marker inside the first user message, and a
// session whose first message carried no text (agent-20261026-12) stayed
// unbound for the whole 150s capture window and then forever: `send_to` to that
// agent resolved to the daemon UUID and nothing ever delivered.
//
// opencode accepts a caller-chosen session id: `opencode run -s ses_ymbind...
// "..."` created exactly that id (verified 27.10.2026 against the running v2
// server). So the daemon names the session before the process starts and binds
// it itself, which removes the guess entirely.

import (
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/storage"
)

func los69Handler(t *testing.T) (*Handler, *storage.Store, string) {
	t.Helper()
	s, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	h := &Handler{store: s}
	id := "agent-20261027-90"
	if err := s.AgentCreate(storage.Agent{
		ID: id, Project: "/p", Section: "los69", SessionID: "daemon-uuid",
		Status: "running", PID: 1, Backend: "opencode",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	return h, s, id
}

func los69Flag(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func TestLos69NewSpawnNamesTheSessionItself(t *testing.T) {
	h, s, id := los69Handler(t)

	args, _ := h.opencodeSpawnPlan(id, "daemon-uuid", false, "", "", "los69", "/usr/bin/opencode", nil)

	sid := los69Flag(args, "-s")
	if sid == "" {
		t.Fatalf("a new spawn must name its session with -s, got args %v", args)
	}
	if !strings.HasPrefix(sid, "ses") {
		t.Errorf("session id %q must start with ses, opencode refuses anything else", sid)
	}
	if sid == "daemon-uuid" {
		t.Errorf("the daemon uuid is not a session id, opencode rejects it")
	}

	a, err := s.AgentGet(id)
	if err != nil || a == nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if a.OpencodeSessionID != sid {
		t.Errorf("agent row carries %q, want %q: the bind must be persisted before the process starts",
			a.OpencodeSessionID, sid)
	}
}

func TestLos69EveryNewSpawnGetsItsOwnSession(t *testing.T) {
	h, s, id := los69Handler(t)

	first, _ := h.opencodeSpawnPlan(id, "daemon-uuid", false, "", "", "los69", "/usr/bin/opencode", nil)
	one := los69Flag(first, "-s")

	// A second agent in the same project: two fresh spawns must never share a
	// session, or the second would silently continue the first conversation.
	other := "agent-20261027-91"
	if err := s.AgentCreate(storage.Agent{
		ID: other, Project: "/p", Section: "los69b", SessionID: "daemon-uuid-2",
		Status: "running", PID: 1, Backend: "opencode",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
	second, _ := h.opencodeSpawnPlan(other, "daemon-uuid-2", false, "", "", "los69b", "/usr/bin/opencode", nil)
	two := los69Flag(second, "-s")

	if one == "" || two == "" {
		t.Fatalf("both spawns must name a session, got %q and %q", one, two)
	}
	if one == two {
		t.Errorf("two new spawns share the session %q", one)
	}
}

func TestLos69ResumeKeepsTheStoredSession(t *testing.T) {
	h, s, id := los69Handler(t)
	if ok, err := s.AgentBindSession(id, "ses_stored01", ""); err != nil || !ok {
		t.Fatalf("AgentBindSession: ok=%v err=%v", ok, err)
	}

	args, _ := h.opencodeSpawnPlan(id, "daemon-uuid", true, "", "", "los69", "/usr/bin/opencode", nil)

	if got := los69Flag(args, "-s"); got != "ses_stored01" {
		t.Errorf("resume must continue the stored session, got %q", got)
	}
}

// Without a store the plan must stay clean: the spawn test for the theme flags
// builds a bare Handler{}, and a nil store must not panic or invent a session.
func TestLos69SpawnPlanWithoutAStoreStaysUsable(t *testing.T) {
	h := &Handler{}
	args, _ := h.opencodeSpawnPlan("agent-x", "daemon-uuid", false, "zai/glm-5.2", "", "sec", "/usr/bin/opencode", nil)
	if los69Flag(args, "-s") != "" {
		t.Errorf("a handler without a store cannot bind, so it must not name a session: %v", args)
	}
	if los69Flag(args, "--model") != "zai/glm-5.2" {
		t.Errorf("the model flag must still be there: %v", args)
	}
}

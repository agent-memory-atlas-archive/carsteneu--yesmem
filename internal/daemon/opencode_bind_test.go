package daemon

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/carsteneu/yesmem/internal/opencode"
	"github.com/carsteneu/yesmem/internal/storage"
)

// stubOpencodeFind replaces the on-demand lookup with a fixed answer and counts
// the calls, so tests neither touch a live service nor wait for one.
func stubOpencodeFind(t *testing.T, sid string) *int32 {
	t.Helper()
	var calls int32
	orig := opencodeFindSessionFn
	opencodeFindSessionFn = func(ctx context.Context, q opencode.MarkerQuery) (string, error) {
		atomic.AddInt32(&calls, 1)
		return sid, nil
	}
	t.Cleanup(func() { opencodeFindSessionFn = orig })
	return &calls
}

func createUnboundOpencodeAgent(t *testing.T, s *storage.Store, id string) {
	t.Helper()
	if err := s.AgentCreate(storage.Agent{
		ID: id, Project: "proj", Section: "sec-" + id,
		SessionID: "uuid-" + id, Status: "running", Backend: "opencode",
	}); err != nil {
		t.Fatalf("AgentCreate: %v", err)
	}
}

func TestEnsureOpencodeSession_BindsAndPersists(t *testing.T) {
	h, s := mustHandler(t)
	calls := stubOpencodeFind(t, "ses_bound")
	createUnboundOpencodeAgent(t, s, "agent-a")

	a, _ := s.AgentGet("agent-a")
	got := h.ensureOpencodeSession(a)
	if got.OpencodeSessionID != "ses_bound" {
		t.Fatalf("returned agent OpencodeSessionID = %q, want ses_bound", got.OpencodeSessionID)
	}
	stored, _ := s.AgentGet("agent-a")
	if stored.OpencodeSessionID != "ses_bound" {
		t.Errorf("persisted OpencodeSessionID = %q, want ses_bound", stored.OpencodeSessionID)
	}
	if *calls != 1 {
		t.Errorf("lookups = %d, want 1", *calls)
	}
}

func TestEnsureOpencodeSession_FailedLookupIsThrottled(t *testing.T) {
	h, s := mustHandler(t)
	calls := stubOpencodeFind(t, "")
	createUnboundOpencodeAgent(t, s, "agent-b")

	a, _ := s.AgentGet("agent-b")
	h.ensureOpencodeSession(a)
	h.ensureOpencodeSession(a)
	h.ensureOpencodeSession(a)
	if *calls != 1 {
		t.Errorf("lookups = %d, want 1 (cooldown after a failed attempt)", *calls)
	}
}

func TestEnsureOpencodeSession_SkipsBoundAndOtherBackends(t *testing.T) {
	h, s := mustHandler(t)
	calls := stubOpencodeFind(t, "ses_x")
	_ = s.AgentCreate(storage.Agent{ID: "bound", Project: "p", Section: "s1", SessionID: "u1", Status: "running", Backend: "opencode", OpencodeSessionID: "ses_have"})
	_ = s.AgentCreate(storage.Agent{ID: "claude", Project: "p", Section: "s2", SessionID: "u2", Status: "running", Backend: "claude"})

	for _, id := range []string{"bound", "claude"} {
		a, _ := s.AgentGet(id)
		h.ensureOpencodeSession(a)
	}
	if *calls != 0 {
		t.Errorf("lookups = %d, want 0", *calls)
	}
}

func TestEnsureOpencodeSession_DoesNotStealAnotherAgentsSession(t *testing.T) {
	h, s := mustHandler(t)
	stubOpencodeFind(t, "ses_taken")
	_ = s.AgentCreate(storage.Agent{ID: "owner", Project: "p", Section: "s1", SessionID: "u1", Status: "running", Backend: "opencode", OpencodeSessionID: "ses_taken"})
	createUnboundOpencodeAgent(t, s, "agent-c")

	a, _ := s.AgentGet("agent-c")
	if got := h.ensureOpencodeSession(a); got.OpencodeSessionID != "" {
		t.Errorf("bound %q, want nothing (session belongs to another agent)", got.OpencodeSessionID)
	}
}

func TestGetAgent_BindsOnDemand(t *testing.T) {
	h, s := mustHandler(t)
	stubOpencodeFind(t, "ses_bound")
	createUnboundOpencodeAgent(t, s, "agent-d")

	resp := h.handleGetAgent(map[string]any{"to": "agent-d"})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
	stored, _ := s.AgentGet("agent-d")
	if stored.OpencodeSessionID != "ses_bound" {
		t.Errorf("OpencodeSessionID = %q, want ses_bound after get_agent", stored.OpencodeSessionID)
	}
	if m := resultMap(t, resp); m["opencode_session_id"] != "ses_bound" {
		t.Errorf("get_agent opencode_session_id = %v, want ses_bound in the response", m["opencode_session_id"])
	}
}

func TestAgentToMap_IncludesBackendSessionIDs(t *testing.T) {
	m := agentToMap(&storage.Agent{ID: "a", OpencodeSessionID: "ses_x", CodexSessionID: "cx-1", WorkDir: "/wd/a"})
	if m["opencode_session_id"] != "ses_x" || m["codex_session_id"] != "cx-1" {
		t.Errorf("agentToMap = opencode %v / codex %v, want ses_x / cx-1", m["opencode_session_id"], m["codex_session_id"])
	}
	// get_agent has to expose the directory a bind is scoped by, or a failed
	// bind cannot be diagnosed without falling back to list_agents.
	if m["work_dir"] != "/wd/a" {
		t.Errorf("agentToMap work_dir = %v, want /wd/a", m["work_dir"])
	}
}

func TestSendTo_AgentIDBindsAndStoresBackendAddress(t *testing.T) {
	h, s := mustHandler(t)
	stubOpencodeFind(t, "ses_bound")
	createUnboundOpencodeAgent(t, s, "agent-e")

	resp := h.handleSendTo(map[string]any{"target": "agent-e", "sender": "orchestrator", "content": "hi"})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
	msgs, _ := s.GetChannelMessages([]string{"opencode:ses_bound"})
	if len(msgs) != 1 {
		t.Errorf("messages under opencode:ses_bound = %d, want 1 (the address the agent's proxy polls)", len(msgs))
	}
}

func TestTrackUsage_UnknownOpencodeThreadBindsItsAgent(t *testing.T) {
	h, s := mustHandler(t)
	stubOpencodeFind(t, "ses_bound")
	createUnboundOpencodeAgent(t, s, "agent-f")

	resp := h.handleTrackUsage(map[string]any{
		"thread_id": "opencode:ses_bound", "project": "proj",
		"input_tokens": float64(100), "output_tokens": float64(10),
	})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
	a, _ := s.AgentGet("agent-f")
	if a.OpencodeSessionID != "ses_bound" || a.TurnsUsed != 1 {
		t.Errorf("OpencodeSessionID=%q TurnsUsed=%d, want ses_bound/1", a.OpencodeSessionID, a.TurnsUsed)
	}
}

func TestTrackUsage_NonAgentThreadGetsNoTelemetry(t *testing.T) {
	h, s := mustHandler(t)
	stubOpencodeFind(t, "ses_agent_own")
	createUnboundOpencodeAgent(t, s, "agent-g")

	h.handleTrackUsage(map[string]any{
		"thread_id": "opencode:ses_orchestrator", "project": "proj",
		"input_tokens": float64(100), "output_tokens": float64(10),
	})
	a, _ := s.AgentGet("agent-g")
	if a.TurnsUsed != 0 {
		t.Errorf("TurnsUsed = %d, want 0 (thread belongs to no agent)", a.TurnsUsed)
	}
}

func TestWhoami_UnboundAgentResolvedOnDemand(t *testing.T) {
	h, s := mustHandler(t)
	stubOpencodeFind(t, "ses_bound")
	createUnboundOpencodeAgent(t, s, "agent-h")

	resp := h.handleWhoami(map[string]any{"session_id": "opencode:ses_bound"})
	if resp.Error != "" {
		t.Fatalf("error: %s", resp.Error)
	}
	m := resultMap(t, resp)
	if m["is_agent"] != true || m["agent_id"] != "agent-h" {
		t.Errorf("whoami = %v, want is_agent=true agent_id=agent-h", m)
	}
}

// TestBindThrottle_KeepsLiveCooldownsAtTheCap: reaching the size cap must not
// drop the cooldowns that are still enforcing a rate limit — dropping them would
// let every unbound agent be scanned again at once. Expired entries go first.
func TestBindThrottle_KeepsLiveCooldownsAtTheCap(t *testing.T) {
	var throttle bindThrottle
	now := time.Now()
	const live = "agent:live"
	throttle.try(live, now)
	for i := 0; i < bindThrottleMaxKeys-1; i++ {
		throttle.try(fmt.Sprintf("expired-%d", i), now.Add(-2*opencodeBindCooldown))
	}
	if len(throttle.last) != bindThrottleMaxKeys {
		t.Fatalf("setup: %d keys, want %d", len(throttle.last), bindThrottleMaxKeys)
	}

	if !throttle.try("agent:new", now) {
		t.Fatal("a key that was never attempted must be allowed")
	}
	if _, ok := throttle.last[live]; !ok {
		t.Error("the live cooldown was dropped by the sweep")
	}
	if len(throttle.last) > 8 {
		t.Errorf("expired entries survived the sweep: %d keys left", len(throttle.last))
	}
}

// TestBindThrottle_ThrottlesWithinTheCooldown pins the behaviour the sweep must
// preserve.
func TestBindThrottle_ThrottlesWithinTheCooldown(t *testing.T) {
	var throttle bindThrottle
	now := time.Now()
	if !throttle.try("agent:a", now) {
		t.Fatal("first attempt must be allowed")
	}
	if throttle.try("agent:a", now.Add(opencodeBindCooldown/2)) {
		t.Error("a second attempt inside the cooldown must be refused")
	}
	if !throttle.try("agent:a", now.Add(opencodeBindCooldown+time.Second)) {
		t.Error("an attempt after the cooldown must be allowed")
	}
}

// useRealOpencodeFind restores the production lookup (TestMain replaces it with
// a fixed empty answer) so a test can drive the marker matching through
// ensureOpencodeSession against a stub service.
func useRealOpencodeFind(t *testing.T) {
	t.Helper()
	orig := opencodeFindSessionFn
	opencodeFindSessionFn = defaultOpencodeFindSession
	t.Cleanup(func() { opencodeFindSessionFn = orig })
}

// TestEnsureOpencodeSession_DoesNotBindTheNextRun reproduces the live defect: two
// agents share a section, three minutes apart. The second agent's session lies
// inside the first agent's window, and the first agent's own session carries no
// user message at all — so a scan that stops at the first section hit binds the
// *sibling's* conversation to the first agent.
//
// The first agent has no evidence of its own (its session never received the
// prompt), so it stays unbound. Binding the one session without a message was the
// rule that produced the swapped rows of 21.10.2026 (YM-419): every agent of that
// wave shared a workdir and their sessions appeared one by one, so each new blank
// session looked like "the only one in the workdir" to every sibling still
// polling. An unbound agent is recoverable (the on-demand and late binds retry as
// soon as a marker exists); a wrong binding is not.
func TestEnsureOpencodeSession_DoesNotBindTheNextRun(t *testing.T) {
	h, s := mustHandler(t)
	isolateHome(t)
	useRealOpencodeFind(t)

	const wd = "/wd/shared-wt"
	now := time.Now()
	// First agent: three minutes old, its own session has no user message.
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-08", Project: "proj", Section: "shared-loop",
		SessionID: "uuid-08", Status: "running", Backend: "opencode",
		WorkDir: wd, CreatedAt: now.Add(-3 * time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate agent-08: %v", err)
	}
	// Second agent of the same section, started now.
	if err := s.AgentCreate(storage.Agent{
		ID: "agent-09", Project: "proj", Section: "shared-loop",
		SessionID: "uuid-09", Status: "running", Backend: "opencode",
		WorkDir: wd, CreatedAt: now.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AgentCreate agent-09: %v", err)
	}

	srv := v2StubServer(t, []v2Session{
		// The sibling's session: section matches, and it sits inside
		// agent-08's window.
		{ID: "ses_run_09", Agent: "build", Dir: wd, Created: now.UnixMilli()},
		// agent-08's own session: created with its agent row, no user message.
		{ID: "ses_run_08", Agent: "build", Dir: wd, Created: now.Add(-3 * time.Minute).UnixMilli()},
	}, wd, map[string]string{
		"ses_run_09": "You are working on project 'proj', agent 'agent-09', section 'shared-loop'.",
	})
	stubV2Service(t, srv)

	a8, _ := s.AgentGet("agent-08")
	if got := h.ensureOpencodeSession(a8); got.OpencodeSessionID != "" {
		t.Fatalf("agent-08 bound %q — with no marker of its own there is nothing to prove the session is its, and the blank rule is what swapped two rows", got.OpencodeSessionID)
	}
	a9, _ := s.AgentGet("agent-09")
	if got := h.ensureOpencodeSession(a9); got.OpencodeSessionID != "ses_run_09" {
		t.Fatalf("agent-09 bound %q, want ses_run_09 via its own marker", got.OpencodeSessionID)
	}
}

// TestBindAgentSession_RefusesAnOwnedSession: the ownership guard is the single
// place that decides whether a session may be written onto an agent, so a second
// agent can never take a session that another row already owns — whatever path
// ("spawn capture", "late bind", "on demand") reports the match.
func TestBindAgentSession_RefusesAnOwnedSession(t *testing.T) {
	h, s := mustHandler(t)
	_ = s.AgentCreate(storage.Agent{
		ID: "owner", Project: "p", Section: "s1", SessionID: "u1", Status: "running",
		Backend: "opencode", OpencodeSessionID: "ses_taken",
	})
	createUnboundOpencodeAgent(t, s, "agent-z")

	other, _ := s.AgentGet("agent-z")
	if h.bindAgentSession(other, "ses_taken", "late bind") {
		t.Fatal("a session owned by another agent must not be bound")
	}
	stored, _ := s.AgentGet("agent-z")
	if stored.OpencodeSessionID != "" {
		t.Errorf("OpencodeSessionID = %q, want empty", stored.OpencodeSessionID)
	}
	if owner, _ := s.AgentGet("owner"); owner.OpencodeSessionID != "ses_taken" {
		t.Errorf("the owner's session changed to %q", owner.OpencodeSessionID)
	}
}

// TestBindAgentSession_ClearsUnresumable: a successful bind is what makes an
// agent resumable again, so the "unresumable" marker must go — while any other
// progress value (a phase, a pause reason) must survive.
func TestBindAgentSession_ClearsUnresumable(t *testing.T) {
	h, s := mustHandler(t)
	for _, tc := range []struct{ id, progress, want string }{
		{"unres", opencodeUnresumableMsg, ""},
		{"phase", "Phase 3/6 EXECUTE", "Phase 3/6 EXECUTE"},
	} {
		if err := s.AgentCreate(storage.Agent{
			ID: tc.id, Project: "p", Section: "s-" + tc.id, SessionID: "u-" + tc.id,
			Status: "running", Backend: "opencode",
		}); err != nil {
			t.Fatalf("AgentCreate %s: %v", tc.id, err)
		}
		// AgentCreate does not persist progress, so it is written separately.
		if err := s.AgentUpdate(tc.id, map[string]any{"progress": tc.progress}); err != nil {
			t.Fatalf("AgentUpdate %s: %v", tc.id, err)
		}
		a, _ := s.AgentGet(tc.id)
		if !h.bindAgentSession(a, "ses_"+tc.id, "on demand") {
			t.Fatalf("%s: bind refused", tc.id)
		}
		stored, _ := s.AgentGet(tc.id)
		if stored.Progress != tc.want {
			t.Errorf("%s progress = %q, want %q", tc.id, stored.Progress, tc.want)
		}
	}
}

// TestBindAgentSession_EmptySessionIsRefused: a path that reports "no match"
// must not be able to blank an agent's session column.
func TestBindAgentSession_EmptySessionIsRefused(t *testing.T) {
	h, s := mustHandler(t)
	_ = s.AgentCreate(storage.Agent{
		ID: "agent-b", Project: "p", Section: "s", SessionID: "u", Status: "running",
		Backend: "opencode", OpencodeSessionID: "ses_keep",
	})
	a, _ := s.AgentGet("agent-b")
	if h.bindAgentSession(a, "", "late bind") {
		t.Fatal("an empty session id must not be bound")
	}
	if stored, _ := s.AgentGet("agent-b"); stored.OpencodeSessionID != "ses_keep" {
		t.Errorf("OpencodeSessionID = %q, want ses_keep", stored.OpencodeSessionID)
	}
}
